package binding

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"

	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

func init() {
	Register(kuryrv1.VIFTypeVHostUser, func(o Options) (Driver, error) {
		if o.VHostUserMountPoint == "" {
			return nil, fmt.Errorf("[vhostuser]mount_point is required by the vhost-user " +
				"binding driver; it is the directory shared with the pod")
		}
		return &vhostUserDriver{opts: o}, nil
	})
}

// vhostUserMode values, from the point of view of the pod (os-vif's
// VIFVHostUser.mode).
const (
	// vhostUserModeServer means the pod creates the socket and OVS connects
	// to it. Nothing has to be moved.
	vhostUserModeServer = "server"
	// vhostUserModeClient means the pod connects to a socket OVS creates in
	// its own directory, which therefore has to be moved to where the pod
	// can see it.
	vhostUserModeClient = "client"
)

// vhostUserConfig is the file left in the shared mount for the application in
// the pod to read.
//
// The field names are the ones the Python implementation wrote, because the
// consumer is the workload, not Kuryr: renaming them would break every
// existing DPDK application manifest.
type vhostUserConfig struct {
	VHostName string `json:"vhostname"`
	VHostMAC  string `json:"vhostmac"`
	Mode      string `json:"mode"`
}

// vhostUserDriver shares an Open vSwitch vhost-user socket with the pod.
//
// There is no interface to create here and nothing to move into a network
// namespace: a vhost-user port is a Unix socket that a userspace dataplane in
// the pod opens directly. All this driver does is put the socket, and a small
// file describing it, where the pod can reach them.
type vhostUserDriver struct {
	opts Options
	// addPort and delPort manage the OVS port behind the socket. They are
	// fields so tests can run without Open vSwitch; nil means the real ones.
	addPort, delPort func(ctx context.Context, bridge string, req Request) error
}

// bridge is the OVS bridge the vhost-user port lives on.
func (d *vhostUserDriver) bridge(req Request) string {
	if req.VIF.Bridge != "" {
		return req.VIF.Bridge
	}
	return d.opts.OVSBridge
}

// Name is the registered driver name, used in logs and metrics.
func (d *vhostUserDriver) Name() string { return "vhostuser" }

// Connect places the vhost-user socket, and the file describing it, where the
// pod can reach them. Open vSwitch owns the socket itself; nothing here is
// plugged into a namespace.
func (d *vhostUserDriver) Connect(ctx context.Context, req Request) error {
	if req.VIF.VHostUser == nil {
		return fmt.Errorf("VIF %s is of type vhostuser but carries no vhost-user configuration",
			req.VIF.ID)
	}

	socketPath := req.VIF.VHostUser.Path
	if socketPath == "" {
		return fmt.Errorf("VIF %s has no vhost-user socket path", req.VIF.ID)
	}
	portName := path.Base(socketPath)

	if err := d.writeConfig(req, portName); err != nil {
		return err
	}

	addPort := d.addPort
	if addPort == nil {
		addPort = addVHostUserPort
	}
	if err := addPort(ctx, d.bridge(req), req); err != nil {
		return fmt.Errorf("creating the OVS vhost-user port of VIF %s: %w", req.VIF.ID, err)
	}

	if req.VIF.VHostUser.Mode == vhostUserModeServer {
		// The pod creates the socket and Open vSwitch connects to it, so
		// there is nothing on this side yet to move.
		return nil
	}

	// Open vSwitch created the socket in its own directory. It has to end up
	// in the mount the pod shares, and OVS cannot be told to put it there.
	source := path.Join(d.opts.OVSVhuPath, portName)
	if err := checkSocket(source); err != nil {
		return fmt.Errorf("VIF %s: %w", req.VIF.ID, err)
	}

	if err := os.Rename(source, socketPath); err != nil {
		return fmt.Errorf("moving the vhost-user socket of VIF %s from %s to %s: %w",
			req.VIF.ID, source, socketPath, err)
	}

	log.FromContext(ctx).V(1).Info("vhost-user socket shared with the pod",
		"vif", req.VIF.ID, "socket", socketPath)
	return nil
}

// Disconnect removes the configuration file the pod was given. The socket
// belongs to Open vSwitch, which tears it down with the port.
func (d *vhostUserDriver) Disconnect(ctx context.Context, req Request) error {
	logger := log.FromContext(ctx)
	configPath := d.configPath(req)

	delPort := d.delPort
	if delPort == nil {
		delPort = delVHostUserPort
	}
	if err := delPort(ctx, d.bridge(req), req); err != nil {
		return fmt.Errorf("removing the OVS vhost-user port of VIF %s: %w", req.VIF.ID, err)
	}

	conf, err := d.readConfig(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Either Connect never ran or a previous Disconnect finished.
			// Both mean there is nothing left to clean up.
			return nil
		}
		return err
	}

	// Open vSwitch cannot delete a socket that was moved out from under it,
	// so removing it is this driver's job.
	socketPath := path.Join(d.opts.VHostUserMountPoint, conf.VHostName)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		// Report it but keep going: leaving the config file behind as well
		// would make the next Connect for this container fail too.
		logger.Error(err, "could not remove the vhost-user socket",
			"vif", req.VIF.ID, "socket", socketPath)
	}

	if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the vhost-user configuration of VIF %s at %s: %w",
			req.VIF.ID, configPath, err)
	}
	return nil
}

// writeConfig leaves the description of the socket next to it.
func (d *vhostUserDriver) writeConfig(req Request, portName string) error {
	conf := vhostUserConfig{
		VHostName: portName,
		VHostMAC:  req.VIF.Address,
		Mode:      req.VIF.VHostUser.Mode,
	}

	encoded, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("encoding the vhost-user configuration of VIF %s: %w", req.VIF.ID, err)
	}

	configPath := d.configPath(req)
	if err := os.WriteFile(configPath, encoded, 0o644); err != nil {
		return fmt.Errorf("writing the vhost-user configuration of VIF %s to %s: %w",
			req.VIF.ID, configPath, err)
	}
	return nil
}

func (d *vhostUserDriver) readConfig(configPath string) (*vhostUserConfig, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var conf vhostUserConfig
	if err := json.Unmarshal(raw, &conf); err != nil {
		return nil, fmt.Errorf("reading the vhost-user configuration at %s: %w", configPath, err)
	}
	if conf.VHostName == "" {
		return nil, fmt.Errorf("the vhost-user configuration at %s names no socket", configPath)
	}
	return &conf, nil
}

// configPath is where the description of one interface lives. Keying it on
// the container and interface name is what lets Disconnect find the socket
// without being told which VIF it belonged to.
func (d *vhostUserDriver) configPath(req Request) string {
	return path.Join(d.opts.VHostUserMountPoint, req.ContainerID+"-"+req.IfName)
}

// checkSocket reports whether a path is a socket that exists.
func checkSocket(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("vhost-user socket %s does not exist yet; Open vSwitch has not created it", p)
		}
		return fmt.Errorf("checking the vhost-user socket %s: %w", p, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists but is not a socket", p)
	}
	return nil
}

// vhostUserPortColumns are the Interface columns os-vif sets for a vhost-user
// port. The mode is the pod's (os-vif's VIFVHostUser.mode): a pod acting as
// client means OVS is the server and creates the socket (dpdkvhostuser); a pod
// acting as server means OVS connects to the socket at the VIF path
// (dpdkvhostuserclient).
func vhostUserPortColumns(v *kuryrv1.VIF) []string {
	if v.VHostUser != nil && v.VHostUser.Mode == vhostUserModeServer {
		return []string{"type=dpdkvhostuserclient", "options:vhost-server-path=" + v.VHostUser.Path}
	}
	return []string{"type=dpdkvhostuser"}
}
