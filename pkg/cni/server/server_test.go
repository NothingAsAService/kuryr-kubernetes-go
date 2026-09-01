package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/cni/binding"
	"opendev.org/openstack/kuryr-kubernetes/pkg/cni/registry"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
)

// fakeBinder records what the server asked to be plugged, without touching
// the node.
type fakeBinder struct {
	mu         sync.Mutex
	connects   []binding.Request
	disconnect []binding.Request
	connectErr error
}

func (f *fakeBinder) Connect(_ context.Context, req binding.Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connectErr != nil {
		return f.connectErr
	}
	f.connects = append(f.connects, req)
	return nil
}

func (f *fakeBinder) Disconnect(_ context.Context, req binding.Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnect = append(f.disconnect, req)
	return nil
}

func (f *fakeBinder) connected() []binding.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]binding.Request(nil), f.connects...)
}

func (f *fakeBinder) disconnected() []binding.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]binding.Request(nil), f.disconnect...)
}

func newTestServer(t *testing.T) (*Server, *registry.Registry, *fakeBinder) {
	t.Helper()

	cfg := config.Defaults()
	cfg.NeutronDefaults.PodSubnet = "subnet-1"
	cfg.NeutronDefaults.Project = "project-1"
	cfg.CNIDaemon.VIFAnnotationTimeout = 300 * time.Millisecond

	reg := registry.New()
	binder := &fakeBinder{}
	srv := New(cfg, reg, logr.Discard(), NewMetrics(prometheus.NewRegistry()))
	srv.binder = binder
	srv.delWait = 100 * time.Millisecond

	return srv, reg, binder
}

func kuryrPort(uid types.UID, active bool) *kuryrv1.KuryrPort {
	return &kuryrv1.KuryrPort{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Spec:       kuryrv1.KuryrPortSpec{PodUID: uid, PodNodeName: "worker-1"},
		Status: kuryrv1.KuryrPortStatus{
			VIFs: map[string]kuryrv1.PortVIF{
				kuryrv1.DefaultIfName: {
					Default: true,
					VIF: kuryrv1.VIF{
						Type:    kuryrv1.VIFTypeOVS,
						ID:      "port-1",
						Address: "fa:16:3e:01:02:03",
						VIFName: "tapport-1",
						Bridge:  "br-int",
						Active:  active,
						Network: kuryrv1.Network{
							ID:  "net-1",
							MTU: 1442,
							Subnets: []kuryrv1.Subnet{{
								ID: "subnet-1", CIDR: "10.0.1.0/24",
								Gateway: "10.0.1.1", IPs: []string{"10.0.1.42"},
							}},
						},
					},
				},
			},
		},
	}
}

func post(t *testing.T, srv *Server, path string, p Params) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshalling params: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/"+path, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	switch path {
	case "addNetwork":
		srv.handleAdd(rec, req)
	case "delNetwork":
		srv.handleDel(rec, req)
	default:
		t.Fatalf("unknown path %q", path)
	}
	return rec
}

func addParams() Params {
	return Params{
		Command:     "ADD",
		ContainerID: "container-1",
		NetNS:       "/proc/1234/ns/net",
		IfName:      "eth0",
		Args:        "K8S_POD_NAMESPACE=default;K8S_POD_NAME=web-0;K8S_POD_UID=uid-1",
	}
}

func TestAddSucceeds(t *testing.T) {
	srv, reg, binder := newTestServer(t)
	reg.Upsert("default/web-0", kuryrPort("uid-1", true))

	rec := post(t, srv, "addNetwork", addParams())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body)
	}

	var vif kuryrv1.VIF
	if err := json.Unmarshal(rec.Body.Bytes(), &vif); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if vif.ID != "port-1" {
		t.Errorf("returned VIF id = %q, want port-1", vif.ID)
	}

	conns := binder.connected()
	if len(conns) != 1 {
		t.Fatalf("connected %d interfaces, want 1", len(conns))
	}
	// The runtime chose the name; the daemon must honour it rather than use
	// the key from the CRD.
	if conns[0].IfName != "eth0" {
		t.Errorf("ifName = %q, want eth0", conns[0].IfName)
	}
	if !conns[0].IsDefaultGateway {
		t.Error("the primary interface must be marked as the default gateway")
	}
	if conns[0].PodUID != "uid-1" {
		t.Errorf("podUID = %q, want uid-1", conns[0].PodUID)
	}

	entry, _ := reg.Get("default/web-0")
	if entry.ContainerID != "container-1" {
		t.Errorf("containerID = %q, want container-1", entry.ContainerID)
	}
}

func TestAddWaitsForActive(t *testing.T) {
	srv, reg, _ := newTestServer(t)
	// Ports exist but Neutron has not activated them.
	reg.Upsert("default/web-0", kuryrPort("uid-1", false))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- post(t, srv, "addNetwork", addParams()) }()

	select {
	case rec := <-done:
		t.Fatalf("ADD returned before the port was ACTIVE (status %d)", rec.Code)
	case <-time.After(80 * time.Millisecond):
	}

	reg.Upsert("default/web-0", kuryrPort("uid-1", true))

	select {
	case rec := <-done:
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ADD never completed after the port became active")
	}
}

func TestAddTimesOutWithTryAgainLater(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rec := post(t, srv, "addNetwork", addParams())
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusGatewayTimeout)
	}

	var e Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	// The runtime keys off this code to decide whether retrying is worthwhile.
	if e.Code != ErrTryAgainLater {
		t.Errorf("code = %d, want %d", e.Code, ErrTryAgainLater)
	}
}

func TestAddOnDeletedPodReportsGone(t *testing.T) {
	srv, reg, _ := newTestServer(t)
	reg.MarkPodGone("default/web-0")

	rec := post(t, srv, "addNetwork", addParams())
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusGone)
	}

	var e Error
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != ErrContainerUnknown {
		t.Errorf("code = %d, want %d", e.Code, ErrContainerUnknown)
	}
}

func TestAddOnUIDMismatchReportsGone(t *testing.T) {
	srv, reg, _ := newTestServer(t)
	reg.Upsert("default/web-0", kuryrPort("uid-old", true))

	rec := post(t, srv, "addNetwork", addParams()) // asks for uid-1
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusGone, rec.Body)
	}
}

func TestAddRejectsMalformedArgs(t *testing.T) {
	srv, _, _ := newTestServer(t)

	p := addParams()
	p.Args = "K8S_POD_NAME=web-0" // no namespace

	rec := post(t, srv, "addNetwork", p)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var e Error
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != ErrInvalidEnvironmentVariables {
		t.Errorf("code = %d, want %d", e.Code, ErrInvalidEnvironmentVariables)
	}
}

func TestDeleteUnplugs(t *testing.T) {
	srv, reg, binder := newTestServer(t)
	reg.Upsert("default/web-0", kuryrPort("uid-1", true))
	reg.SetContainerID("default/web-0", "container-1")

	p := addParams()
	p.Command = "DEL"

	rec := post(t, srv, "delNetwork", p)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNoContent, rec.Body)
	}
	if got := binder.disconnected(); len(got) != 1 {
		t.Fatalf("disconnected %d interfaces, want 1", len(got))
	}
}

func TestDeleteIgnoresSupersededAdd(t *testing.T) {
	// A DEL naming an older container belongs to a failed ADD. Acting on it
	// would rip the network out from under the pod that is running now.
	srv, reg, binder := newTestServer(t)
	reg.Upsert("default/web-0", kuryrPort("uid-1", true))
	reg.SetContainerID("default/web-0", "container-CURRENT")

	p := addParams()
	p.Command = "DEL"
	p.ContainerID = "container-OLD"

	rec := post(t, srv, "delNetwork", p)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := binder.disconnected(); len(got) != 0 {
		t.Fatalf("disconnected %d interfaces; a superseded DEL must be ignored", len(got))
	}
}

func TestDeleteWithoutRegistryDataSucceeds(t *testing.T) {
	// Nothing is known about the pod. Failing here would leave it stuck in
	// Terminating, so the daemon reports success and lets the controller's
	// garbage collector deal with any Neutron leftovers.
	srv, _, _ := newTestServer(t)

	p := addParams()
	p.Command = "DEL"

	rec := post(t, srv, "delNetwork", p)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNoContent, rec.Body)
	}
}

func TestHealthTripsAfterRepeatedFailures(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if !srv.Healthy() {
		t.Fatal("a fresh server should be healthy")
	}

	// Each ADD times out because nothing populates the registry.
	for i := 0; i < srv.cfg.CNIDaemon.CNIFailuresCount; i++ {
		post(t, srv, "addNetwork", addParams())
	}

	if srv.Healthy() {
		t.Errorf("after %d consecutive failures the daemon should report unhealthy",
			srv.cfg.CNIDaemon.CNIFailuresCount)
	}
}

func TestHealthRecoversAfterSuccess(t *testing.T) {
	srv, reg, _ := newTestServer(t)

	post(t, srv, "addNetwork", addParams()) // one failure
	reg.Upsert("default/web-0", kuryrPort("uid-1", true))

	rec := post(t, srv, "addNetwork", addParams())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if srv.consecutiveFailures.Load() != 0 {
		t.Error("a success should reset the consecutive failure counter")
	}
}

func TestParseArgs(t *testing.T) {
	args := ParseArgs("IgnoreUnknown=1;K8S_POD_NAMESPACE=kube-system;K8S_POD_NAME=coredns-abc;" +
		"K8S_POD_UID=1234;K8S_POD_INFRA_CONTAINER_ID=sandbox-9")

	if args.Namespace != "kube-system" {
		t.Errorf("namespace = %q, want kube-system", args.Namespace)
	}
	if args.Name != "coredns-abc" {
		t.Errorf("name = %q, want coredns-abc", args.Name)
	}
	if args.UID != "1234" {
		t.Errorf("uid = %q, want 1234", args.UID)
	}
	if args.InfraContainerID != "sandbox-9" {
		t.Errorf("infra container = %q, want sandbox-9", args.InfraContainerID)
	}
}

func TestParseArgsToleratesMissingUID(t *testing.T) {
	// Some runtimes do not pass K8S_POD_UID. The server has to cope, because
	// rejecting would break CNI entirely on those.
	args := ParseArgs("K8S_POD_NAMESPACE=default;K8S_POD_NAME=web-0")
	if args.UID != "" {
		t.Errorf("uid = %q, want empty", args.UID)
	}
	if args.Namespace != "default" || args.Name != "web-0" {
		t.Errorf("args = %+v, want namespace default and name web-0", args)
	}
}

func TestAddWithoutPodUIDSkipsTheCheck(t *testing.T) {
	srv, reg, _ := newTestServer(t)
	reg.Upsert("default/web-0", kuryrPort("uid-whatever", true))

	p := addParams()
	p.Args = "K8S_POD_NAMESPACE=default;K8S_POD_NAME=web-0"

	rec := post(t, srv, "addNetwork", p)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body)
	}
}
