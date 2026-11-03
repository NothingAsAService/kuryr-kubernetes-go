// Package binding attaches and detaches pod interfaces on the node.
//
// This replaces kuryr_kubernetes/cni/binding/*.py, which drove pyroute2 and
// delegated part of the work to os-vif plugins. There is no os-vif in Go, so
// the plugging is done directly here with netlink, which also removes the
// awkward split where os-vif created the port and the Kuryr driver then moved
// it into the namespace.
//
// Everything that touches netlink lives behind a linux build tag. The
// interface and the registry are portable so the rest of the daemon, and the
// tests, build anywhere.
package binding

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
)

// Request is one interface to attach or detach.
type Request struct {
	// VIF is the binding information the controller produced.
	VIF *kuryrv1.VIF
	// IfName is the name the interface must have inside the pod, as asked
	// for by the runtime in CNI_IFNAME.
	IfName string
	// NetNS is the pod network namespace path from CNI_NETNS.
	NetNS string
	// ContainerID is CNI_CONTAINERID.
	ContainerID string
	// IsDefaultGateway marks the interface that receives the default route.
	// Only the primary interface gets one; additional interfaces from the
	// multi-VIF drivers must not fight over it.
	IsDefaultGateway bool
	// PodUID is used as the OVS vm-uuid external id, which is what lets an
	// operator trace a bridge port back to a pod.
	PodUID string
}

// Driver attaches one kind of VIF.
type Driver interface {
	// Name is the registered driver name.
	Name() string
	// Connect creates the interface and moves it into the pod namespace.
	// It must be safe to call again after a partial failure.
	Connect(ctx context.Context, req Request) error
	// Disconnect tears the interface down. It must treat an already-absent
	// interface as success, because the runtime retries DEL freely.
	Disconnect(ctx context.Context, req Request) error
}

// Options are the node-wide settings the drivers need.
type Options struct {
	// LinkIface pins the node uplink for nested drivers. Empty means
	// auto-detect.
	LinkIface string
	// NetNSProcDir is where the host /proc is mounted when the daemon runs
	// in a container. Empty means the daemon shares the host mount
	// namespace and paths need no translation.
	NetNSProcDir string
	// OVSBridge is the integration bridge fallback.
	OVSBridge string
	// VHostUserMountPoint is where vhost-user sockets live.
	VHostUserMountPoint string
	// OVSVhuPath is where Open vSwitch keeps its vhost-user sockets.
	OVSVhuPath string
	// DPDKDriver is the userspace driver a nested-DPDK device is rebound to.
	DPDKDriver string
	// DPDKPCIMountPoint is where the PCI address of each device is written
	// for the pod to read.
	DPDKPCIMountPoint string
}

// OptionsFromConfig lifts the binding settings out of kuryr.conf.
func OptionsFromConfig(cfg *config.Config) Options {
	return Options{
		LinkIface:           cfg.Binding.LinkIface,
		NetNSProcDir:        netnsProcDir(cfg),
		OVSBridge:           cfg.NeutronDefaults.OVSBridge,
		VHostUserMountPoint: cfg.VHostUser.MountPoint,
		OVSVhuPath:          cfg.VHostUser.OVSVhuPath,
		DPDKDriver:          cfg.NestedDPDK.Driver,
		DPDKPCIMountPoint:   cfg.NestedDPDK.PCIMountPoint,
	}
}

func netnsProcDir(cfg *config.Config) string {
	if cfg.CNIDaemon.DockerMode {
		return cfg.CNIDaemon.NetnsProcDir
	}
	return ""
}

type factory func(Options) (Driver, error)

var (
	registryMu sync.RWMutex
	registry   = map[kuryrv1.VIFType]factory{}
)

// Register adds a driver for a VIF type.
func Register(t kuryrv1.VIFType, f factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[t] = f
}

// For returns the driver that binds a given VIF type.
func For(v *kuryrv1.VIF, opts Options) (Driver, error) {
	// The available types are only assembled on the failure path, and the
	// factory runs after the lock is released.
	registryMu.RLock()
	f, ok := registry[v.Type]
	var available []kuryrv1.VIFType
	if !ok {
		available = slices.Sorted(maps.Keys(registry))
	}
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("no binding driver for VIF type %q; available: %v", v.Type, available)
	}
	return f(opts)
}

// Connect binds one interface, choosing the driver from the VIF type.
func Connect(ctx context.Context, req Request, opts Options) error {
	drv, err := For(req.VIF, opts)
	if err != nil {
		return err
	}
	if err := drv.Connect(ctx, req); err != nil {
		return fmt.Errorf("%s: connecting %s: %w", drv.Name(), req.VIF.ID, err)
	}
	if req.VIF.NeedsL3Config() {
		if err := ConfigureL3With(req, opts); err != nil {
			return fmt.Errorf("%s: configuring L3 for %s: %w", drv.Name(), req.VIF.ID, err)
		}
	}
	return nil
}

// Disconnect unbinds one interface.
func Disconnect(ctx context.Context, req Request, opts Options) error {
	drv, err := For(req.VIF, opts)
	if err != nil {
		return err
	}
	if err := drv.Disconnect(ctx, req); err != nil {
		return fmt.Errorf("%s: disconnecting %s: %w", drv.Name(), req.VIF.ID, err)
	}
	return nil
}

// ResolveNetNS translates a namespace path for a daemon running inside a
// container with the host /proc bind-mounted elsewhere.
//
// The runtime hands out paths like /proc/1234/ns/net that are meaningful in
// the host mount namespace. A containerised daemon sees its own /proc, so
// without this it would look up the wrong process, or none.
func (o Options) ResolveNetNS(netns string) string {
	if o.NetNSProcDir == "" {
		return netns
	}
	if !strings.HasPrefix(netns, "/proc/") {
		// cri-o style paths (/var/run/netns/...) are file paths in a mount
		// the daemon shares already, so they need no translation.
		return netns
	}
	// path.Join, not filepath.Join: a netns path is always a Linux kernel
	// path, whatever platform this happens to be compiled on.
	return path.Join(o.NetNSProcDir, strings.TrimPrefix(netns, "/proc/"))
}

// maxIfNameLen is the kernel's IFNAMSIZ minus the terminating NUL.
const maxIfNameLen = 14

// hybridVethNames returns the qvb (bridge side) and qvo (OVS side) ends of
// the veth pair joining a port's qbr bridge to the integration bridge, named
// the way os-vif and the Neutron OVS agent expect.
func hybridVethNames(portID string) (qvb, qvo string) {
	trim := func(s string) string {
		if len(s) > maxIfNameLen {
			return s[:maxIfNameLen]
		}
		return s
	}
	return trim("qvb" + portID), trim("qvo" + portID)
}
