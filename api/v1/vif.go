package v1

import (
	"fmt"
	"strings"
)

// Interface name limits inherited from Neutron/OVS conventions. A Linux
// interface name is capped at IFNAMSIZ-1 = 15 bytes, but Neutron has always
// truncated to 14 so that tap<11 hex> matches what the OVS agent expects.
const (
	nicNameLen = 14

	hostIfPrefix      = "tap"
	containerIfPrefix = "t_"
	hybridBridgePfx   = "qbr"
	vhostUserPrefix   = "vhu"
)

// VIFType discriminates the concrete kind of VIF carried in a VIF struct.
//
// The Python implementation modelled this as an os-vif class hierarchy
// (VIFOpenVSwitch, VIFBridge, VIFVlanNested, ...) serialized through
// oslo.versionedobjects. A class name buried in a JSON blob cannot be
// validated by an OpenAPI schema and cannot be deep-copied by generated code,
// so the Go rewrite makes the discriminator an explicit enumerated field.
//
// +kubebuilder:validation:Enum=ovs;bridge;nested-vlan;nested-macvlan;nested-dpdk;vhostuser
type VIFType string

const (
	// VIFTypeOVS is a port plugged straight into the integration bridge.
	VIFTypeOVS VIFType = "ovs"
	// VIFTypeBridge is a port plugged into a Linux bridge (OVS hybrid plug).
	VIFTypeBridge VIFType = "bridge"
	// VIFTypeNestedVLAN is a trunk subport surfaced as a VLAN subinterface
	// inside the node VM.
	VIFTypeNestedVLAN VIFType = "nested-vlan"
	// VIFTypeNestedMacvlan is a macvlan child of the node VM uplink.
	VIFTypeNestedMacvlan VIFType = "nested-macvlan"
	// VIFTypeNestedDPDK is a port handed to a userspace driver inside the pod.
	VIFTypeNestedDPDK VIFType = "nested-dpdk"
	// VIFTypeVHostUser is a vhost-user socket shared with the pod.
	VIFTypeVHostUser VIFType = "vhostuser"
)

// Route is a static route pushed into the pod network namespace.
type Route struct {
	// Destination is the CIDR the route applies to.
	Destination string `json:"destination"`
	// NextHop is the gateway address for Destination.
	NextHop string `json:"nextHop"`
}

// Subnet is one Neutron subnet as seen from a pod.
//
// Unlike the os-vif Subnet this carries the subnet ID. The Python code kept
// the ID only in the enclosing map key, which forced every consumer to thread
// that map along with the object.
type Subnet struct {
	// ID is the Neutron subnet UUID.
	ID string `json:"id,omitempty"`
	// CIDR is the subnet range, for example 10.0.1.0/24.
	CIDR string `json:"cidr"`
	// Gateway is the subnet gateway address, empty when the subnet has none.
	Gateway string `json:"gateway,omitempty"`
	// DNS holds the subnet nameservers.
	DNS []string `json:"dns,omitempty"`
	// Routes holds the subnet host routes.
	Routes []Route `json:"routes,omitempty"`
	// IPs are the addresses allocated to this port within the subnet.
	IPs []string `json:"ips,omitempty"`
}

// Network is the Neutron network a VIF belongs to.
type Network struct {
	// ID is the Neutron network UUID.
	ID string `json:"id"`
	// Name is the human readable network name.
	Name string `json:"name,omitempty"`
	// MTU is the network MTU. The CNI binding refuses to plug a VIF whose MTU
	// exceeds the node uplink MTU, so this must be accurate.
	MTU int32 `json:"mtu,omitempty"`
	// Subnets are the subnets this port holds addresses on.
	Subnets []Subnet `json:"subnets,omitempty"`
}

// VHostUserConfig carries the vhost-user specific attributes.
type VHostUserConfig struct {
	// Path is the socket path shared with the pod.
	Path string `json:"path"`
	// Mode is client or server, from the hypervisor point of view.
	Mode string `json:"mode,omitempty"`
}

// DPDKConfig carries the nested-DPDK specific attributes.
type DPDKConfig struct {
	// PCIAddress is the address of the device handed to the pod.
	PCIAddress string `json:"pciAddress,omitempty"`
	// DevDriver is the userspace driver the device is rebound to.
	DevDriver string `json:"devDriver,omitempty"`
	// L3Setup tells the CNI whether to configure addresses and routes. A DPDK
	// pod usually programs its own dataplane instead.
	L3Setup bool `json:"l3Setup,omitempty"`
}

// VIF is everything the CNI daemon needs to attach one interface to a pod.
//
// This replaces the os-vif VIFBase hierarchy. Two fields from the Python model
// are deliberately gone:
//
//   - plugin: it selected an os-vif plugin ("ovs" or the kuryr no-op). The Go
//     daemon does its own plugging, so nothing reads it.
//   - port_profile.interface_id: it always held the Neutron port ID, which is
//     already in ID.
//
// One field is new. ParentPortID records the trunk a subport hangs off. The
// Python cleanup path had to scan every trunk in the project to rediscover it
// (utils.get_parent_port_id); recording it at creation turns that scan into a
// lookup.
type VIF struct {
	// Type selects how this VIF is bound on the node.
	Type VIFType `json:"type"`
	// ID is the Neutron port UUID.
	ID string `json:"id"`
	// Address is the port MAC address.
	Address string `json:"address"`
	// VIFName is the host-side interface name derived from ID.
	VIFName string `json:"vifName,omitempty"`
	// Active reports whether Neutron has moved the port to ACTIVE. The CNI
	// daemon holds the ADD call until this flips true.
	Active bool `json:"active"`
	// HasTrafficFiltering reports whether Neutron applies security groups on
	// the port itself.
	HasTrafficFiltering bool `json:"hasTrafficFiltering,omitempty"`
	// PreserveOnDelete keeps the host interface when the pod goes away.
	PreserveOnDelete bool `json:"preserveOnDelete,omitempty"`

	// Network is the network and subnets this port lives on.
	Network Network `json:"network"`

	// Bridge is the OVS or Linux bridge to plug into. Set for the ovs and
	// bridge types only.
	Bridge string `json:"bridge,omitempty"`
	// VLANID is the trunk segmentation id. Set for nested-vlan only.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	VLANID uint16 `json:"vlanID,omitempty"`
	// ParentPortID is the trunk parent port. Set for nested types only.
	ParentPortID string `json:"parentPortID,omitempty"`

	// VHostUser is set for the vhostuser type only.
	VHostUser *VHostUserConfig `json:"vhostUser,omitempty"`
	// DPDK is set for the nested-dpdk type only.
	DPDK *DPDKConfig `json:"dpdk,omitempty"`
}

// IsNested reports whether the VIF is bound inside a node VM rather than on
// bare metal.
func (v *VIF) IsNested() bool {
	switch v.Type {
	case VIFTypeNestedVLAN, VIFTypeNestedMacvlan, VIFTypeNestedDPDK:
		return true
	default:
		return false
	}
}

// NeedsL3Config reports whether the CNI should assign addresses and routes in
// the pod namespace. A vhost-user pod gets a socket rather than an interface,
// and a DPDK pod usually programs its own dataplane.
func (v *VIF) NeedsL3Config() bool {
	switch v.Type {
	case VIFTypeVHostUser:
		return false
	case VIFTypeNestedDPDK:
		return v.DPDK != nil && v.DPDK.L3Setup
	default:
		return true
	}
}

// FixedIPs flattens the per-subnet addresses into a single list.
func (v *VIF) FixedIPs() []string {
	var ips []string
	for i := range v.Network.Subnets {
		ips = append(ips, v.Network.Subnets[i].IPs...)
	}
	return ips
}

// HostIfaceName returns the host-side interface name for a Neutron port,
// matching what the Neutron OVS agent expects to find on the bridge.
func HostIfaceName(portID string) string {
	return truncateIfName(hostIfPrefix + portID)
}

// ContainerIfaceName returns the temporary name used for the pod side of a
// veth pair before it is renamed to the CNI-requested name.
func ContainerIfaceName(portID string) string {
	return truncateIfName(containerIfPrefix + portID)
}

// HybridBridgeName returns the Linux bridge name used by the OVS hybrid plug.
func HybridBridgeName(portID string) string {
	return truncateIfName(hybridBridgePfx + portID)
}

// VHostUserIfaceName returns the socket basename for a vhost-user port.
func VHostUserIfaceName(portID string) string {
	return truncateIfName(vhostUserPrefix + portID)
}

func truncateIfName(name string) string {
	if len(name) > nicNameLen {
		return name[:nicNameLen]
	}
	return name
}

// Validate performs the structural checks an OpenAPI schema cannot express,
// such as "a nested-vlan VIF must carry a VLAN id".
func (v *VIF) Validate() error {
	if v.ID == "" {
		return fmt.Errorf("vif: empty port id")
	}
	if v.Address == "" {
		return fmt.Errorf("vif %s: empty MAC address", v.ID)
	}
	if v.Network.ID == "" {
		return fmt.Errorf("vif %s: empty network id", v.ID)
	}
	if len(v.Network.Subnets) == 0 {
		return fmt.Errorf("vif %s: no subnets", v.ID)
	}
	switch v.Type {
	case VIFTypeNestedVLAN:
		if v.VLANID == 0 || v.VLANID > 4094 {
			return fmt.Errorf("vif %s: vlan id %d out of range 1-4094", v.ID, v.VLANID)
		}
	case VIFTypeOVS, VIFTypeBridge:
		if v.Bridge == "" {
			return fmt.Errorf("vif %s: type %s requires a bridge", v.ID, v.Type)
		}
	case VIFTypeVHostUser:
		if v.VHostUser == nil || v.VHostUser.Path == "" {
			return fmt.Errorf("vif %s: type vhostuser requires a socket path", v.ID)
		}
	case VIFTypeNestedMacvlan, VIFTypeNestedDPDK:
		// No extra structural requirements.
	default:
		return fmt.Errorf("vif %s: unknown type %q", v.ID, v.Type)
	}
	return nil
}

// String renders a VIF compactly for logs.
func (v *VIF) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s mac=%s", v.Type, v.ID, v.Address)
	if v.VLANID != 0 {
		fmt.Fprintf(&b, " vlan=%d", v.VLANID)
	}
	if ips := v.FixedIPs(); len(ips) > 0 {
		fmt.Fprintf(&b, " ips=%s", strings.Join(ips, ","))
	}
	if v.Active {
		b.WriteString(" active")
	}
	return b.String()
}
