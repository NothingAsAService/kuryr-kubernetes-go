// Package vif turns Neutron ports into the VIF objects the CNI daemon acts on.
//
// This is the Go replacement for kuryr_kubernetes/os_vif_util.py. The Python
// version dispatched through stevedore into os-vif object constructors; here
// the mapping is a plain function per VIF type, which makes it testable
// without a cloud and readable without knowing os-vif.
package vif

import (
	"fmt"
	"maps"
	"path"
	"slices"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

// Keys Neutron uses inside binding:vif_details.
const (
	detailBridgeName    = "bridge_name"
	detailPortFilter    = "port_filter"
	detailOVSHybridPlug = "ovs_hybrid_plug"
	detailVHostUserMode = "vhostuser_mode"
)

// Options carries the deployment-wide settings the conversion needs.
type Options struct {
	// DefaultOVSBridge is used when Neutron does not name one itself.
	DefaultOVSBridge string
	// VHostUserMountPoint is where vhost-user sockets are created.
	VHostUserMountPoint string
}

// SubnetSet is the set of subnets a port may hold addresses on, keyed by
// subnet ID. It mirrors what a subnet driver returns.
type SubnetSet map[string]*openstack.Subnet

// NetworkID returns the network every subnet in the set belongs to, and
// whether the set was consistent about it. Kuryr assumes a pod port lives on
// exactly one network, and a mismatch means a driver returned nonsense.
func (s SubnetSet) NetworkID() (string, bool) {
	var id string
	for _, sub := range s {
		if id == "" {
			id = sub.NetworkID
			continue
		}
		if sub.NetworkID != id {
			return "", false
		}
	}
	return id, id != ""
}

// FromPort builds the VIF for a port bound on bare metal, choosing between
// the OVS, hybrid-bridge and vhost-user shapes the way Neutron asks for.
func FromPort(port *openstack.Port, subnets SubnetSet, network *openstack.Network, opts Options) (*kuryrv1.VIF, error) {
	base, err := baseVIF(port, subnets, network)
	if err != nil {
		return nil, err
	}

	bridge := port.StringFromDetails(detailBridgeName)
	if bridge == "" {
		bridge = opts.DefaultOVSBridge
	}

	switch {
	case port.StringFromDetails(detailVHostUserMode) != "":
		if opts.VHostUserMountPoint == "" {
			return nil, fmt.Errorf("port %s is vhost-user but [vhostuser]mount_point is unset", port.ID)
		}
		name := kuryrv1.VHostUserIfaceName(port.ID)
		base.Type = kuryrv1.VIFTypeVHostUser
		base.VIFName = name
		base.Bridge = bridge
		base.VHostUser = &kuryrv1.VHostUserConfig{
			Path: path.Join(opts.VHostUserMountPoint, name),
			Mode: port.StringFromDetails(detailVHostUserMode),
		}

	case port.BoolFromDetails(detailOVSHybridPlug):
		// Neutron wants the port behind a Linux bridge so iptables rules can
		// be applied to it; the veth goes into qbr<port-id> rather than
		// straight into br-int.
		base.Type = kuryrv1.VIFTypeBridge
		base.VIFName = kuryrv1.HostIfaceName(port.ID)
		base.Bridge = kuryrv1.HybridBridgeName(port.ID)

	default:
		if bridge == "" {
			return nil, fmt.Errorf(
				"port %s: no bridge in binding:vif_details and [neutron_defaults]ovs_bridge is unset",
				port.ID)
		}
		base.Type = kuryrv1.VIFTypeOVS
		base.VIFName = kuryrv1.HostIfaceName(port.ID)
		base.Bridge = bridge
	}

	if err := base.Validate(); err != nil {
		return nil, err
	}
	return base, nil
}

// FromPortNestedVLAN builds the VIF for a trunk subport, which becomes a VLAN
// subinterface of the node VM uplink.
func FromPortNestedVLAN(port *openstack.Port, subnets SubnetSet, network *openstack.Network,
	vlanID int, parentPortID string) (*kuryrv1.VIF, error) {

	base, err := baseVIF(port, subnets, network)
	if err != nil {
		return nil, err
	}
	if vlanID < 1 || vlanID > 4094 {
		return nil, fmt.Errorf("port %s: vlan id %d out of range 1-4094", port.ID, vlanID)
	}

	base.Type = kuryrv1.VIFTypeNestedVLAN
	base.VIFName = kuryrv1.HostIfaceName(port.ID)
	base.VLANID = uint16(vlanID)
	base.ParentPortID = parentPortID

	if err := base.Validate(); err != nil {
		return nil, err
	}
	return base, nil
}

// FromPortNestedMacvlan builds the VIF for a macvlan child of the node VM
// uplink. There is no trunk involved, so there is no VLAN id to allocate.
func FromPortNestedMacvlan(port *openstack.Port, subnets SubnetSet, network *openstack.Network) (*kuryrv1.VIF, error) {
	base, err := baseVIF(port, subnets, network)
	if err != nil {
		return nil, err
	}
	base.Type = kuryrv1.VIFTypeNestedMacvlan
	base.VIFName = kuryrv1.HostIfaceName(port.ID)

	if err := base.Validate(); err != nil {
		return nil, err
	}
	return base, nil
}

// FromPortNestedDPDK builds the VIF for a port handed to a userspace driver
// inside the pod.
func FromPortNestedDPDK(port *openstack.Port, subnets SubnetSet, network *openstack.Network) (*kuryrv1.VIF, error) {
	base, err := baseVIF(port, subnets, network)
	if err != nil {
		return nil, err
	}
	base.Type = kuryrv1.VIFTypeNestedDPDK
	base.VIFName = kuryrv1.HostIfaceName(port.ID)
	// PCI address and driver are filled in on the node, where the device is
	// actually visible; L3 is left to the pod.
	base.DPDK = &kuryrv1.DPDKConfig{L3Setup: false}

	if err := base.Validate(); err != nil {
		return nil, err
	}
	return base, nil
}

// baseVIF fills the attributes every VIF type shares.
func baseVIF(port *openstack.Port, subnets SubnetSet, network *openstack.Network) (*kuryrv1.VIF, error) {
	if port == nil {
		return nil, fmt.Errorf("vif: nil port")
	}
	if network == nil {
		return nil, fmt.Errorf("vif: nil network for port %s", port.ID)
	}
	if port.NetworkID != network.ID {
		return nil, fmt.Errorf("port %s belongs to network %s but was given network %s",
			port.ID, port.NetworkID, network.ID)
	}

	vifSubnets, err := buildSubnets(port, subnets)
	if err != nil {
		return nil, err
	}

	return &kuryrv1.VIF{
		ID:                  port.ID,
		Address:             port.MACAddress,
		Active:              port.IsActive(),
		HasTrafficFiltering: port.BoolFromDetails(detailPortFilter),
		PreserveOnDelete:    false,
		Network: kuryrv1.Network{
			ID:      network.ID,
			Name:    network.Name,
			MTU:     int32(network.MTU),
			Subnets: vifSubnets,
		},
	}, nil
}

// buildSubnets groups the port fixed IPs by subnet and copies the subnet
// attributes the pod needs (gateway, DNS, routes).
//
// A port with no address on any of the requested subnets is an error rather
// than an empty VIF: the pod would come up with an interface and no way to
// reach anything, which is far harder to diagnose than a failed CNI ADD.
func buildSubnets(port *openstack.Port, subnets SubnetSet) ([]kuryrv1.Subnet, error) {
	// Preserve a stable order so that repeated conversions of the same port
	// produce byte-identical CRD status and do not churn resourceVersion.
	var order []string
	grouped := map[string]*kuryrv1.Subnet{}

	for _, fip := range port.FixedIPs {
		sub, ok := subnets[fip.SubnetID]
		if !ok {
			continue
		}
		entry, seen := grouped[fip.SubnetID]
		if !seen {
			entry = &kuryrv1.Subnet{
				ID:      sub.ID,
				CIDR:    sub.CIDR,
				Gateway: sub.GatewayIP,
				DNS:     append([]string(nil), sub.DNS...),
			}
			for _, r := range sub.HostRoutes {
				entry.Routes = append(entry.Routes, kuryrv1.Route{
					Destination: r.Destination,
					NextHop:     r.NextHop,
				})
			}
			grouped[fip.SubnetID] = entry
			order = append(order, fip.SubnetID)
		}
		if fip.IPAddress != "" {
			entry.IPs = append(entry.IPs, fip.IPAddress)
		}
	}

	if len(order) == 0 {
		return nil, fmt.Errorf("port %s has no addresses on any of the requested subnets", port.ID)
	}

	out := make([]kuryrv1.Subnet, 0, len(order))
	for _, id := range order {
		out = append(out, *grouped[id])
	}
	return out, nil
}

// FixedIPsFor turns a subnet set into the fixed_ips body of a port create
// request, asking Neutron to allocate one address per subnet.
func FixedIPsFor(subnets SubnetSet) []openstack.FixedIP {
	// Sorted for determinism: an unordered map would make otherwise identical
	// bulk create requests differ between calls.
	out := make([]openstack.FixedIP, 0, len(subnets))
	for _, id := range slices.Sorted(maps.Keys(subnets)) {
		out = append(out, openstack.FixedIP{SubnetID: id})
	}
	return out
}
