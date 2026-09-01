package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/cni/binding"
	"opendev.org/openstack/kuryr-kubernetes/pkg/cni/registry"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
)

// CNI error codes. These are the values the Python daemon used and the
// runtime interprets, so they are part of the contract rather than internal.
const (
	// ErrContainerUnknown tells the runtime the sandbox is gone and it
	// should stop asking.
	ErrContainerUnknown = 3
	// ErrInvalidEnvironmentVariables means the request was malformed.
	ErrInvalidEnvironmentVariables = 4
	// ErrTryAgainLater means the state was not ready; the runtime retries.
	ErrTryAgainLater = 11
	// ErrInternal is everything else.
	ErrInternal = 999
)

// defaultDelWaitTimeout bounds how long DEL waits for registry data.
//
// Waiting long on a DEL is pointless: if the VIF information is gone there is
// nothing to reconstruct it from, and blocking leaves the pod stuck
// Terminating. It is not zero only to cover a daemon restart, where the
// watcher needs a moment to repopulate.
const defaultDelWaitTimeout = 5 * time.Second

// Error is the JSON body the plugin expects on failure.
type Error struct {
	Code    uint   `json:"code"`
	Msg     string `json:"msg"`
	Details string `json:"details,omitempty"`
}

// Binder is the part of the CNI path that touches the node.
//
// It exists as an interface so the server can be tested without root, netlink
// or a network namespace. The production implementation is a thin wrapper
// around the binding package.
type Binder interface {
	Connect(ctx context.Context, req binding.Request) error
	Disconnect(ctx context.Context, req binding.Request) error
}

// netlinkBinder is the real implementation.
type netlinkBinder struct{ opts binding.Options }

// Connect plugs the interface, choosing a driver from the VIF type.
func (b netlinkBinder) Connect(ctx context.Context, req binding.Request) error {
	return binding.Connect(ctx, req, b.opts)
}

// Disconnect unplugs it again.
func (b netlinkBinder) Disconnect(ctx context.Context, req binding.Request) error {
	return binding.Disconnect(ctx, req, b.opts)
}

// Server answers CNI ADD and DEL on a loopback socket.
type Server struct {
	registry *registry.Registry
	opts     binding.Options
	binder   Binder
	cfg      *config.Config
	// delWait is how long DEL waits for registry data before giving up.
	delWait time.Duration
	log     logr.Logger

	// consecutiveFailures drives the health signal. A daemon that cannot
	// plug anything should be restarted rather than left to fail every pod
	// on the node.
	consecutiveFailures atomic.Int32
	healthy             atomic.Bool

	metrics *Metrics
	http    *http.Server
}

// New builds a server.
func New(cfg *config.Config, reg *registry.Registry, log logr.Logger, metrics *Metrics) *Server {
	s := &Server{
		registry: reg,
		opts:     binding.OptionsFromConfig(cfg),
		binder:   netlinkBinder{opts: binding.OptionsFromConfig(cfg)},
		cfg:      cfg,
		log:      log,
		metrics:  metrics,
		delWait:  defaultDelWaitTimeout,
	}
	s.healthy.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /addNetwork", s.handleAdd)
	mux.HandleFunc("POST /delNetwork", s.handleDel)

	s.http = &http.Server{
		Addr:    cfg.CNIDaemon.BindAddress,
		Handler: mux,
		// A CNI ADD legitimately blocks while Neutron activates the port, so
		// the write timeout has to exceed the activation timeout.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      cfg.CNIDaemon.VIFAnnotationTimeout + 60*time.Second,
	}
	return s
}

// Healthy reports whether the daemon believes it can still serve requests.
func (s *Server) Healthy() bool { return s.healthy.Load() }

// Start serves until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.http.Addr, err)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
	}()

	s.log.Info("CNI daemon listening", "address", s.http.Addr)
	if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	params, args, ok := s.decode(w, r, "ADD")
	if !ok {
		return
	}

	key := registry.Key(args.Namespace, args.Name)
	log := s.log.WithValues("pod", key, "containerID", params.ContainerID)

	vif, err := s.add(r.Context(), params, args, key, log)
	s.metrics.Observe("ADD", err, time.Since(start))

	if err != nil {
		s.writeAddError(w, log, key, err)
		return
	}

	s.consecutiveFailures.Store(0)
	log.Info("pod interface ready", "vif", vif.String())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(vif)
}

// add performs the whole ADD sequence for one pod.
func (s *Server) add(ctx context.Context, params Params, args K8sArgs,
	key string, log logr.Logger) (*kuryrv1.VIF, error) {

	// Serialise against a concurrent DEL for the same pod. Different pods on
	// the node proceed in parallel.
	unlock := s.registry.Lock(key)
	defer unlock()

	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.CNIDaemon.VIFAnnotationTimeout)
	defer cancel()

	entry, err := s.registry.WaitForVIFs(waitCtx, key, args.UID)
	if err != nil {
		return nil, err
	}

	for ifname, vif := range entry.VIFs {
		req := s.bindRequest(params, args, ifname, vif)
		if err := s.binder.Connect(ctx, req); err != nil {
			return nil, err
		}
		log.V(1).Info("plugged interface", "ifname", req.IfName, "portID", vif.ID)
	}

	// Record which ADD owns these interfaces before waiting, so that a DEL
	// arriving during the wait can tell whether it belongs to this call.
	s.registry.SetContainerID(key, params.ContainerID)

	// Hold the call open until Neutron has the port ACTIVE. Returning here
	// would let the pod start on an interface whose flows are not programmed,
	// which looks like random packet loss rather than a failure.
	activeCtx, cancelActive := context.WithTimeout(ctx, s.cfg.CNIDaemon.VIFAnnotationTimeout)
	defer cancelActive()

	entry, err = s.registry.WaitForActive(activeCtx, key)
	if err != nil {
		return nil, err
	}

	vif, ok := entry.VIFs[kuryrv1.DefaultIfName]
	if !ok {
		return nil, fmt.Errorf("no %s interface was provisioned for the pod", kuryrv1.DefaultIfName)
	}
	return &vif, nil
}

func (s *Server) handleDel(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	params, args, ok := s.decode(w, r, "DEL")
	if !ok {
		return
	}

	key := registry.Key(args.Namespace, args.Name)
	log := s.log.WithValues("pod", key, "containerID", params.ContainerID)

	err := s.del(r.Context(), params, args, key, log)
	s.metrics.Observe("DEL", err, time.Since(start))

	if err != nil {
		// Missing VIF information on a DEL is not worth failing over. There
		// is nothing left to act on, and refusing would wedge the pod in
		// Terminating forever. The controller garbage collector is what
		// catches any Neutron port left behind.
		if errors.Is(err, registry.ErrTimeout) || errors.Is(err, registry.ErrPodGone) || isUIDMismatch(err) {
			log.Info("no binding information for this pod; treating DEL as done", "reason", err)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.recordFailure()
		log.Error(err, "DEL failed")
		writeError(w, http.StatusInternalServerError, ErrInternal, "error processing delNetwork", err.Error())
		return
	}

	s.consecutiveFailures.Store(0)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) del(ctx context.Context, params Params, args K8sArgs, key string, log logr.Logger) error {
	unlock := s.registry.Lock(key)
	defer unlock()

	if entry, ok := s.registry.Get(key); ok {
		// A DEL naming a different container belongs to an earlier, failed
		// ADD. Acting on it would unplug the pod that is running now.
		if entry.ContainerID != "" && params.ContainerID != "" &&
			entry.ContainerID != params.ContainerID {
			log.Info("ignoring DEL from a superseded ADD",
				"ownerContainerID", entry.ContainerID)
			return nil
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, s.delWait)
	defer cancel()

	entry, err := s.registry.WaitForVIFs(waitCtx, key, args.UID)
	if err != nil {
		// Even without VIF information, remove whatever is in the namespace.
		// Leaving a VLAN subinterface behind makes the next pod on this node
		// fail with a VLAN id conflict.
		s.bestEffortCleanup(ctx, params, log)
		return err
	}

	for ifname, vif := range entry.VIFs {
		req := s.bindRequest(params, args, ifname, vif)
		if err := s.binder.Disconnect(ctx, req); err != nil {
			return err
		}
		log.V(1).Info("unplugged interface", "ifname", req.IfName, "portID", vif.ID)
	}

	s.registry.MarkUnplugged(key)
	return nil
}

// bestEffortCleanup removes the interface by name when the VIF is unknown.
func (s *Server) bestEffortCleanup(ctx context.Context, params Params, log logr.Logger) {
	if params.NetNS == "" {
		return
	}
	stub := &kuryrv1.VIF{Type: kuryrv1.VIFTypeNestedVLAN}
	req := binding.Request{
		VIF:         stub,
		IfName:      params.IfName,
		NetNS:       params.NetNS,
		ContainerID: params.ContainerID,
	}
	if err := s.binder.Disconnect(ctx, req); err != nil {
		log.V(1).Info("best-effort namespace cleanup did not succeed", "error", err)
	}
}

// bindRequest builds the binding request for one interface.
func (s *Server) bindRequest(params Params, args K8sArgs, ifname string, vif kuryrv1.VIF) binding.Request {
	isDefault := ifname == kuryrv1.DefaultIfName
	// The runtime names the primary interface; additional interfaces keep the
	// name the controller assigned them.
	name := ifname
	if isDefault {
		name = params.IfName
	}
	v := vif
	return binding.Request{
		VIF:              &v,
		IfName:           name,
		NetNS:            params.NetNS,
		ContainerID:      params.ContainerID,
		IsDefaultGateway: isDefault,
		PodUID:           string(args.UID),
	}
}

// decode reads and validates a request body.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, command string) (Params, K8sArgs, bool) {
	var params Params
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&params); err != nil {
		s.log.Error(err, "could not decode CNI request")
		writeError(w, http.StatusBadRequest, ErrInvalidEnvironmentVariables,
			"required CNI parameters missing", err.Error())
		return params, K8sArgs{}, false
	}
	if params.Command == "" {
		params.Command = command
	}

	args, err := params.Validate()
	if err != nil {
		s.log.Error(err, "invalid CNI request")
		writeError(w, http.StatusBadRequest, ErrInvalidEnvironmentVariables,
			"required CNI parameters missing", err.Error())
		return params, args, false
	}
	return params, args, true
}

// writeAddError maps an ADD failure onto the status and code the runtime
// knows how to act on.
func (s *Server) writeAddError(w http.ResponseWriter, log logr.Logger, key string, err error) {
	switch {
	case errors.Is(err, registry.ErrPodGone), isUIDMismatch(err):
		// The pod went away while we were working. Retrying cannot help.
		log.Info("pod disappeared during ADD", "reason", err)
		writeError(w, http.StatusGone, ErrContainerUnknown, err.Error(), "")

	case errors.Is(err, registry.ErrTimeout):
		s.recordFailure()
		log.Error(err, "timed out waiting for the controller to provision ports")
		writeError(w, http.StatusGatewayTimeout, ErrTryAgainLater,
			fmt.Sprintf("timed out waiting for ports for %s; check kuryr-controller logs", key),
			err.Error())

	default:
		s.recordFailure()
		log.Error(err, "ADD failed")
		writeError(w, http.StatusInternalServerError, ErrInternal,
			"error processing addNetwork", err.Error())
	}
}

// recordFailure trips the health flag once failures pile up.
func (s *Server) recordFailure() {
	n := s.consecutiveFailures.Add(1)
	if int(n) >= s.cfg.CNIDaemon.CNIFailuresCount {
		if s.healthy.Swap(false) {
			s.log.Error(nil, "too many consecutive CNI failures, reporting unhealthy",
				"failures", n, "threshold", s.cfg.CNIDaemon.CNIFailuresCount)
		}
	}
}

func writeError(w http.ResponseWriter, status int, code uint, msg, details string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Error{Code: code, Msg: msg, Details: details})
}

func isUIDMismatch(err error) bool {
	var e *registry.ErrUIDMismatch
	return errors.As(err, &e)
}
