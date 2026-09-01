//go:build linux

package binding

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

func init() {
	Register(kuryrv1.VIFTypeOVS, func(o Options) (Driver, error) { return &ovsDriver{opts: o}, nil })
	Register(kuryrv1.VIFTypeBridge, func(o Options) (Driver, error) { return &bridgeDriver{opts: o}, nil })
	Register(kuryrv1.VIFTypeNestedVLAN, func(o Options) (Driver, error) { return &vlanDriver{opts: o}, nil })
	Register(kuryrv1.VIFTypeNestedMacvlan, func(o Options) (Driver, error) { return &macvlanDriver{opts: o}, nil })
}

// --- shared helpers ----------------------------------------------------

// removeLinkIfPresent deletes an interface, ignoring one that is already gone.
//
// Every connect path calls this first. A daemon restart between creating an
// interface and returning to the runtime leaves the interface behind, and the
// retry would otherwise fail with EEXIST forever.
func removeLinkIfPresent(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return err
	}
	return netlink.LinkDel(link)
}

// removeLinksInNetNS clears leftover interfaces inside a pod namespace.
func (o Options) removeLinksInNetNS(netns string, names ...string) error {
	path := o.ResolveNetNS(netns)
	return ns.WithNetNSPath(path, func(ns.NetNS) error {
		for _, name := range names {
			if name == "" {
				continue
			}
			if err := removeLinkIfPresent(name); err != nil {
				return fmt.Errorf("removing leftover %s: %w", name, err)
			}
		}
		return nil
	})
}

// moveAndFinish moves a host-side interface into the pod namespace, renames
// it to what the runtime asked for, and brings it up.
func (o Options) moveAndFinish(tempName string, req Request) error {
	link, err := netlink.LinkByName(tempName)
	if err != nil {
		return fmt.Errorf("looking up %s: %w", tempName, err)
	}

	path := o.ResolveNetNS(req.NetNS)
	handle, err := ns.GetNS(path)
	if err != nil {
		return fmt.Errorf("opening netns %s: %w", path, err)
	}
	defer func() { _ = handle.Close() }()

	if err := netlink.LinkSetNsFd(link, int(handle.Fd())); err != nil {
		return fmt.Errorf("moving %s into netns: %w", tempName, err)
	}

	return handle.Do(func(ns.NetNS) error {
		inner, err := netlink.LinkByName(tempName)
		if err != nil {
			return fmt.Errorf("looking up %s inside netns: %w", tempName, err)
		}
		if err := netlink.LinkSetName(inner, req.IfName); err != nil {
			return fmt.Errorf("renaming %s to %s: %w", tempName, req.IfName, err)
		}
		// Re-resolve: the handle is stale after a rename.
		inner, err = netlink.LinkByName(req.IfName)
		if err != nil {
			return fmt.Errorf("looking up %s after rename: %w", req.IfName, err)
		}
		if mtu := int(req.VIF.Network.MTU); mtu > 0 {
			if err := netlink.LinkSetMTU(inner, mtu); err != nil {
				return fmt.Errorf("setting MTU %d on %s: %w", mtu, req.IfName, err)
			}
		}
		if mac, err := net.ParseMAC(req.VIF.Address); err == nil {
			if err := netlink.LinkSetHardwareAddr(inner, mac); err != nil {
				return fmt.Errorf("setting MAC on %s: %w", req.IfName, err)
			}
		}
		return netlink.LinkSetUp(inner)
	})
}

// --- ovs ---------------------------------------------------------------

// ovsDriver plugs a veth pair, one end into the integration bridge.
type ovsDriver struct{ opts Options }

// Name is the registered driver name, used in logs and metrics.
func (d *ovsDriver) Name() string { return "ovs" }

// Connect makes a veth pair, moves one end into the pod namespace and plugs
// the other into the integration bridge with ovs-vsctl.
func (d *ovsDriver) Connect(ctx context.Context, req Request) error {
	hostName := req.VIF.VIFName
	if hostName == "" {
		hostName = kuryrv1.HostIfaceName(req.VIF.ID)
	}
	peerName := kuryrv1.ContainerIfaceName(req.VIF.ID)

	// Clear anything a previous attempt left behind, on both sides.
	if err := d.opts.removeLinksInNetNS(req.NetNS, peerName, req.IfName); err != nil {
		return err
	}
	if err := removeLinkIfPresent(hostName); err != nil {
		return fmt.Errorf("removing leftover host interface %s: %w", hostName, err)
	}

	mtu := int(req.VIF.Network.MTU)
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostName, MTU: mtu},
		PeerName:  peerName,
	}
	if mtu > 0 {
		veth.PeerMTU = uint32(mtu)
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("creating veth %s/%s: %w", hostName, peerName, err)
	}

	// From here on, any failure must not leave half a veth pair behind.
	cleanup := func() { _ = removeLinkIfPresent(hostName) }

	host, err := netlink.LinkByName(hostName)
	if err != nil {
		cleanup()
		return fmt.Errorf("looking up %s: %w", hostName, err)
	}
	if err := netlink.LinkSetUp(host); err != nil {
		cleanup()
		return fmt.Errorf("bringing up %s: %w", hostName, err)
	}

	bridge := req.VIF.Bridge
	if bridge == "" {
		bridge = d.opts.OVSBridge
	}
	if err := ovsAddPort(ctx, bridge, hostName, req); err != nil {
		cleanup()
		return err
	}

	if err := d.opts.moveAndFinish(peerName, req); err != nil {
		_ = ovsDelPort(ctx, bridge, hostName)
		cleanup()
		return err
	}
	return nil
}

// Disconnect removes the port from the integration bridge and deletes the veth
// pair. Deleting either end takes both.
func (d *ovsDriver) Disconnect(ctx context.Context, req Request) error {
	hostName := req.VIF.VIFName
	if hostName == "" {
		hostName = kuryrv1.HostIfaceName(req.VIF.ID)
	}
	bridge := req.VIF.Bridge
	if bridge == "" {
		bridge = d.opts.OVSBridge
	}

	// Remove the bridge port first: deleting the veth while OVS still holds
	// the interface leaves a stale port record on the bridge.
	if err := ovsDelPort(ctx, bridge, hostName); err != nil {
		return err
	}
	// Deleting either end of a veth removes both.
	return removeLinkIfPresent(hostName)
}

// ovsAddPort attaches an interface to the integration bridge with the
// external ids the Neutron OVS agent looks for.
//
// os-vif did this through its own OVSDB handling. Shelling out to ovs-vsctl
// keeps the daemon free of an OVSDB client and matches what an operator would
// type to check the result. iface-id is the important one: it is how the agent
// matches a bridge port to a Neutron port and programs the flows.
func ovsAddPort(ctx context.Context, bridge, ifname string, req Request) error {
	if bridge == "" {
		return fmt.Errorf("no OVS bridge configured for port %s", req.VIF.ID)
	}
	args := []string{
		"--timeout=10", "--may-exist", "add-port", bridge, ifname,
		"--", "set", "Interface", ifname,
		"external_ids:iface-id=" + req.VIF.ID,
		"external_ids:iface-status=active",
		"external_ids:attached-mac=" + req.VIF.Address,
	}
	if req.PodUID != "" {
		args = append(args, "external_ids:vm-uuid="+req.PodUID)
	}
	return runOVSVsctl(ctx, args...)
}

func ovsDelPort(ctx context.Context, bridge, ifname string) error {
	if bridge == "" {
		return nil
	}
	return runOVSVsctl(ctx, "--timeout=10", "--if-exists", "del-port", bridge, ifname)
}

func runOVSVsctl(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ovs-vsctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ovs-vsctl %v: %w: %s", args, err, string(out))
	}
	return nil
}

// --- linux bridge (OVS hybrid plug) ------------------------------------

// bridgeDriver plugs a veth pair into a Linux bridge, which is what Neutron
// asks for when it wants iptables rules applied to the port.
type bridgeDriver struct{ opts Options }

// Name is the registered driver name, used in logs and metrics.
func (d *bridgeDriver) Name() string { return "bridge" }

// Connect makes a veth pair, moves one end into the pod namespace and enslaves
// the other to the Linux bridge Neutron named.
func (d *bridgeDriver) Connect(_ context.Context, req Request) error {
	hostName := req.VIF.VIFName
	if hostName == "" {
		hostName = kuryrv1.HostIfaceName(req.VIF.ID)
	}
	peerName := kuryrv1.ContainerIfaceName(req.VIF.ID)

	if err := d.opts.removeLinksInNetNS(req.NetNS, peerName, req.IfName); err != nil {
		return err
	}
	if err := removeLinkIfPresent(hostName); err != nil {
		return err
	}

	bridge, err := netlink.LinkByName(req.VIF.Bridge)
	if err != nil {
		return fmt.Errorf("bridge %s not found: %w", req.VIF.Bridge, err)
	}

	mtu := int(req.VIF.Network.MTU)
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostName, MTU: mtu, MasterIndex: bridge.Attrs().Index},
		PeerName:  peerName,
	}
	if mtu > 0 {
		veth.PeerMTU = uint32(mtu)
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("creating veth %s/%s: %w", hostName, peerName, err)
	}

	host, err := netlink.LinkByName(hostName)
	if err != nil {
		_ = removeLinkIfPresent(hostName)
		return err
	}
	if err := netlink.LinkSetUp(host); err != nil {
		_ = removeLinkIfPresent(hostName)
		return err
	}

	if err := d.opts.moveAndFinish(peerName, req); err != nil {
		_ = removeLinkIfPresent(hostName)
		return err
	}
	return nil
}

// Disconnect deletes the host end of the veth pair, which takes the pod end
// and the bridge membership with it.
func (d *bridgeDriver) Disconnect(_ context.Context, req Request) error {
	hostName := req.VIF.VIFName
	if hostName == "" {
		hostName = kuryrv1.HostIfaceName(req.VIF.ID)
	}
	return removeLinkIfPresent(hostName)
}

// --- nested vlan -------------------------------------------------------

// vlanDriver creates an 802.1Q subinterface of the node VM uplink and moves
// it into the pod namespace. The compute host sees only the trunk parent.
type vlanDriver struct{ opts Options }

// Name is the registered driver name, used in logs and metrics.
func (d *vlanDriver) Name() string { return "nested-vlan" }

// Connect creates an 802.1Q subinterface of the node VM uplink and moves it
// into the pod namespace.
func (d *vlanDriver) Connect(_ context.Context, req Request) error {
	tempName := req.VIF.VIFName
	if tempName == "" {
		tempName = kuryrv1.HostIfaceName(req.VIF.ID)
	}

	if err := d.opts.removeLinksInNetNS(req.NetNS, tempName, req.IfName); err != nil {
		return err
	}
	if err := removeLinkIfPresent(tempName); err != nil {
		return err
	}

	uplink, err := detectUplink(d.opts.LinkIface)
	if err != nil {
		return err
	}

	// Refuse rather than create a black hole. If the pod network MTU exceeds
	// the uplink MTU, small packets flow and large ones vanish, which
	// presents as TLS handshakes hanging while ping works.
	if mtu := int(req.VIF.Network.MTU); mtu > 0 && uplink.Attrs().MTU < mtu {
		return fmt.Errorf(
			"uplink %s has MTU %d but pod network %s has MTU %d; "+
				"the pod network MTU must not exceed the node MTU",
			uplink.Attrs().Name, uplink.Attrs().MTU, req.VIF.Network.ID, mtu)
	}

	vlan := &netlink.Vlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        tempName,
			ParentIndex: uplink.Attrs().Index,
			MTU:         int(req.VIF.Network.MTU),
		},
		VlanId:       int(req.VIF.VLANID),
		VlanProtocol: netlink.VLAN_PROTOCOL_8021Q,
	}

	if err := netlink.LinkAdd(vlan); err != nil {
		if !errors.Is(err, unixEEXIST) {
			return fmt.Errorf("creating vlan %d on %s: %w",
				req.VIF.VLANID, uplink.Attrs().Name, err)
		}
		// The runtime sometimes issues a new ADD before the old namespace is
		// reaped, which leaves the VLAN id taken. Hunt down the offender and
		// retry once.
		if err := cleanupConflictingVLAN(d.opts, uplink.Attrs().Index, int(req.VIF.VLANID)); err != nil {
			return fmt.Errorf("vlan id %d is in use and could not be reclaimed: %w",
				req.VIF.VLANID, err)
		}
		if err := netlink.LinkAdd(vlan); err != nil {
			return fmt.Errorf("creating vlan %d after reclaiming it: %w", req.VIF.VLANID, err)
		}
	}

	if err := d.opts.moveAndFinish(tempName, req); err != nil {
		_ = removeLinkIfPresent(tempName)
		return err
	}
	return nil
}

// Disconnect removes the subinterface from both the namespace and the host, so
// the VLAN id is free before the next pod asks for one.
func (d *vlanDriver) Disconnect(_ context.Context, req Request) error {
	tempName := req.VIF.VIFName
	if tempName == "" {
		tempName = kuryrv1.HostIfaceName(req.VIF.ID)
	}
	// The interface normally dies with the namespace, but the runtime may
	// create the next pod before that happens, so remove it explicitly to
	// keep the VLAN id free.
	if err := d.opts.removeLinksInNetNS(req.NetNS, tempName, req.IfName); err != nil {
		return err
	}
	return removeLinkIfPresent(tempName)
}

// --- nested macvlan ----------------------------------------------------

// macvlanDriver creates a macvlan child of the node VM uplink. There is no
// trunk and no VLAN id, so no 4094 limit, but Neutron cannot filter it.
type macvlanDriver struct{ opts Options }

// Name is the registered driver name, used in logs and metrics.
func (d *macvlanDriver) Name() string { return "nested-macvlan" }

// Connect creates a macvlan child of the node VM uplink and moves it into the
// pod namespace.
func (d *macvlanDriver) Connect(_ context.Context, req Request) error {
	tempName := req.VIF.VIFName
	if tempName == "" {
		tempName = kuryrv1.HostIfaceName(req.VIF.ID)
	}

	if err := d.opts.removeLinksInNetNS(req.NetNS, tempName, req.IfName); err != nil {
		return err
	}
	if err := removeLinkIfPresent(tempName); err != nil {
		return err
	}

	uplink, err := detectUplink(d.opts.LinkIface)
	if err != nil {
		return err
	}

	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        tempName,
			ParentIndex: uplink.Attrs().Index,
			MTU:         int(req.VIF.Network.MTU),
		},
		Mode: netlink.MACVLAN_MODE_BRIDGE,
	}
	if err := netlink.LinkAdd(mv); err != nil {
		return fmt.Errorf("creating macvlan on %s: %w", uplink.Attrs().Name, err)
	}

	if err := d.opts.moveAndFinish(tempName, req); err != nil {
		_ = removeLinkIfPresent(tempName)
		return err
	}
	return nil
}

// Disconnect removes the macvlan child from the namespace and the host.
func (d *macvlanDriver) Disconnect(_ context.Context, req Request) error {
	tempName := req.VIF.VIFName
	if tempName == "" {
		tempName = kuryrv1.HostIfaceName(req.VIF.ID)
	}
	if err := d.opts.removeLinksInNetNS(req.NetNS, tempName, req.IfName); err != nil {
		return err
	}
	return removeLinkIfPresent(tempName)
}
