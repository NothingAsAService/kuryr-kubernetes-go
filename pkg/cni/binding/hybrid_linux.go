//go:build linux

package binding

import (
	"context"
	"fmt"
	"os"

	"github.com/vishvananda/netlink"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// plugHybrid builds what os-vif's plug() builds for an OVS hybrid port before
// Python's binding driver runs: the qbr<id> Linux bridge the agent applies
// iptables rules on, and a qvb/qvo veth pair joining it to the integration
// bridge, with qvo registered on OVS under the Neutron port ID. Every step
// tolerates what an earlier attempt left in place.
//
// shortcut: qvo goes on [neutron_defaults]ovs_bridge, not on the bridge named
// in binding:vif_details, which the VIF does not carry for this type; record
// it on the VIF if a deployment uses a different integration bridge.
func plugHybrid(ctx context.Context, integration string, req Request) error {
	mtu := int(req.VIF.Network.MTU)
	if err := ensureLinuxBridge(req.VIF.Bridge, mtu); err != nil {
		return err
	}
	bridge, err := netlink.LinkByName(req.VIF.Bridge)
	if err != nil {
		return fmt.Errorf("looking up bridge %s: %w", req.VIF.Bridge, err)
	}

	qvb, qvo := hybridVethNames(req.VIF.ID)
	if _, err := netlink.LinkByName(qvo); err != nil {
		veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: qvb, MTU: mtu}, PeerName: qvo}
		if mtu > 0 {
			veth.PeerMTU = uint32(mtu)
		}
		if err := netlink.LinkAdd(veth); err != nil {
			return fmt.Errorf("creating veth %s/%s: %w", qvb, qvo, err)
		}
	}
	for _, name := range []string{qvb, qvo} {
		link, err := netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("looking up %s: %w", name, err)
		}
		if name == qvb {
			if err := netlink.LinkSetMaster(link, bridge); err != nil {
				return fmt.Errorf("adding %s to bridge %s: %w", qvb, req.VIF.Bridge, err)
			}
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("bringing up %s: %w", name, err)
		}
	}
	return ovsAddPort(ctx, integration, qvo, req)
}

// unplugHybrid takes down what plugHybrid built.
func unplugHybrid(ctx context.Context, integration string, req Request) error {
	_, qvo := hybridVethNames(req.VIF.ID)
	if err := ovsDelPort(ctx, integration, qvo); err != nil {
		return err
	}
	// Deleting either end of the veth removes both.
	if err := removeLinkIfPresent(qvo); err != nil {
		return fmt.Errorf("removing %s: %w", qvo, err)
	}
	if err := removeLinkIfPresent(req.VIF.Bridge); err != nil {
		return fmt.Errorf("removing bridge %s: %w", req.VIF.Bridge, err)
	}
	return nil
}

// ensureLinuxBridge creates a bridge if it is missing and brings it up, set
// the way os-vif's ensure_bridge sets it: no forwarding delay, no STP, and no
// IPv6 on the bridge itself, which would otherwise autoconfigure an address
// and answer on the pod's segment.
func ensureLinuxBridge(name string, mtu int) error {
	if _, err := netlink.LinkByName(name); err != nil {
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: mtu}}
		if err := netlink.LinkAdd(br); err != nil {
			return fmt.Errorf("creating bridge %s: %w", name, err)
		}
	}
	// Best effort, as in os-vif: a kernel without these knobs still forwards.
	_ = os.WriteFile("/sys/class/net/"+name+"/bridge/forward_delay", []byte("0"), 0o644)
	_ = os.WriteFile("/sys/class/net/"+name+"/bridge/stp_state", []byte("0"), 0o644)
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+name+"/disable_ipv6", []byte("1"), 0o644)

	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("looking up bridge %s: %w", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bringing up bridge %s: %w", name, err)
	}
	return nil
}

// addVHostUserPort creates the OVS port behind a vhost-user VIF, as os-vif's
// plug() did before Python's binding driver ran. In client mode this is what
// makes OVS create the socket the driver then moves.
//
// shortcut: the bridge is assumed to exist with datapath_type=netdev; os-vif
// also created it. Add an ensure step if nodes come up without one.
func addVHostUserPort(ctx context.Context, bridge string, req Request) error {
	return ovsAddPort(ctx, bridge, kuryrv1.VHostUserIfaceName(req.VIF.ID), req,
		vhostUserPortColumns(req.VIF)...)
}

// delVHostUserPort removes the OVS port behind a vhost-user VIF.
func delVHostUserPort(ctx context.Context, bridge string, req Request) error {
	return ovsDelPort(ctx, bridge, kuryrv1.VHostUserIfaceName(req.VIF.ID))
}
