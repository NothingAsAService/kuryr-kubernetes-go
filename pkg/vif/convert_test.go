package vif

import (
	"testing"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

const (
	testPortID = "a3f1c2d4-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	testNetID  = "9b2c1a0f-1111-2222-3333-444455556666"
	testSubID  = "5d4c3b2a-aaaa-bbbb-cccc-ddddeeeeffff"
)

func testNetwork() *openstack.Network {
	return &openstack.Network{ID: testNetID, Name: "pod-net", MTU: 1442}
}

func testSubnets() SubnetSet {
	return SubnetSet{
		testSubID: {
			ID:        testSubID,
			NetworkID: testNetID,
			CIDR:      "10.0.1.0/24",
			GatewayIP: "10.0.1.1",
			IPVersion: 4,
			DNS:       []string{"10.0.1.2"},
			HostRoutes: []openstack.HostRoute{
				{Destination: "192.168.0.0/16", NextHop: "10.0.1.254"},
			},
		},
	}
}

func testPort(details map[string]any) *openstack.Port {
	return &openstack.Port{
		ID:         testPortID,
		NetworkID:  testNetID,
		Status:     openstack.PortStatusActive,
		MACAddress: "fa:16:3e:11:22:33",
		FixedIPs:   []openstack.FixedIP{{SubnetID: testSubID, IPAddress: "10.0.1.42"}},
		VIFType:    "ovs",
		VIFDetails: details,
	}
}

func TestFromPortOVS(t *testing.T) {
	v, err := FromPort(testPort(map[string]any{"port_filter": true}), testSubnets(),
		testNetwork(), Options{DefaultOVSBridge: "br-int"})
	if err != nil {
		t.Fatalf("FromPort: %v", err)
	}

	if v.Type != kuryrv1.VIFTypeOVS {
		t.Errorf("type = %q, want %q", v.Type, kuryrv1.VIFTypeOVS)
	}
	if v.Bridge != "br-int" {
		t.Errorf("bridge = %q, want br-int", v.Bridge)
	}
	if !v.Active {
		t.Error("expected VIF to be active for an ACTIVE port")
	}
	if !v.HasTrafficFiltering {
		t.Error("expected port_filter to carry through to HasTrafficFiltering")
	}
	// tap + first 11 characters of the UUID, capped at 14 bytes.
	if want := "tapa3f1c2d4-5e"; v.VIFName != want {
		t.Errorf("vifName = %q, want %q", v.VIFName, want)
	}
	if got := v.FixedIPs(); len(got) != 1 || got[0] != "10.0.1.42" {
		t.Errorf("fixed IPs = %v, want [10.0.1.42]", got)
	}
	if v.Network.MTU != 1442 {
		t.Errorf("MTU = %d, want 1442", v.Network.MTU)
	}

	sub := v.Network.Subnets[0]
	if sub.Gateway != "10.0.1.1" {
		t.Errorf("gateway = %q, want 10.0.1.1", sub.Gateway)
	}
	if len(sub.Routes) != 1 || sub.Routes[0].NextHop != "10.0.1.254" {
		t.Errorf("routes = %+v, want one route via 10.0.1.254", sub.Routes)
	}
	if len(sub.DNS) != 1 || sub.DNS[0] != "10.0.1.2" {
		t.Errorf("dns = %v, want [10.0.1.2]", sub.DNS)
	}
}

func TestFromPortHybridPlug(t *testing.T) {
	v, err := FromPort(testPort(map[string]any{"ovs_hybrid_plug": true}), testSubnets(),
		testNetwork(), Options{DefaultOVSBridge: "br-int"})
	if err != nil {
		t.Fatalf("FromPort: %v", err)
	}
	if v.Type != kuryrv1.VIFTypeBridge {
		t.Errorf("type = %q, want %q", v.Type, kuryrv1.VIFTypeBridge)
	}
	// Hybrid plug puts the veth into a per-port Linux bridge, not br-int.
	if want := "qbra3f1c2d4-5e"; v.Bridge != want {
		t.Errorf("bridge = %q, want %q", v.Bridge, want)
	}
}

func TestFromPortVHostUser(t *testing.T) {
	v, err := FromPort(testPort(map[string]any{"vhostuser_mode": "client"}), testSubnets(),
		testNetwork(), Options{DefaultOVSBridge: "br-int", VHostUserMountPoint: "/var/cni/vhostuser"})
	if err != nil {
		t.Fatalf("FromPort: %v", err)
	}
	if v.Type != kuryrv1.VIFTypeVHostUser {
		t.Errorf("type = %q, want %q", v.Type, kuryrv1.VIFTypeVHostUser)
	}
	if v.VHostUser == nil || v.VHostUser.Mode != "client" {
		t.Fatalf("vhostUser = %+v, want mode client", v.VHostUser)
	}
	if v.NeedsL3Config() {
		t.Error("a vhost-user VIF must not get L3 configuration from the CNI")
	}
}

func TestFromPortVHostUserWithoutMountPoint(t *testing.T) {
	_, err := FromPort(testPort(map[string]any{"vhostuser_mode": "client"}), testSubnets(),
		testNetwork(), Options{DefaultOVSBridge: "br-int"})
	if err == nil {
		t.Fatal("expected an error when mount_point is unset")
	}
}

func TestFromPortNoBridgeConfigured(t *testing.T) {
	_, err := FromPort(testPort(nil), testSubnets(), testNetwork(), Options{})
	if err == nil {
		t.Fatal("expected an error when neither Neutron nor config names a bridge")
	}
}

func TestFromPortNestedVLAN(t *testing.T) {
	v, err := FromPortNestedVLAN(testPort(nil), testSubnets(), testNetwork(), 42, "parent-port-id")
	if err != nil {
		t.Fatalf("FromPortNestedVLAN: %v", err)
	}
	if v.Type != kuryrv1.VIFTypeNestedVLAN {
		t.Errorf("type = %q, want %q", v.Type, kuryrv1.VIFTypeNestedVLAN)
	}
	if v.VLANID != 42 {
		t.Errorf("vlanID = %d, want 42", v.VLANID)
	}
	if v.ParentPortID != "parent-port-id" {
		t.Errorf("parentPortID = %q, want parent-port-id", v.ParentPortID)
	}
	if !v.IsNested() {
		t.Error("nested-vlan must report IsNested")
	}
}

func TestFromPortNestedVLANRejectsBadID(t *testing.T) {
	for _, id := range []int{0, 4095, -1} {
		if _, err := FromPortNestedVLAN(testPort(nil), testSubnets(), testNetwork(), id, "p"); err == nil {
			t.Errorf("vlan id %d: expected an error", id)
		}
	}
}

func TestFromPortRejectsNetworkMismatch(t *testing.T) {
	port := testPort(nil)
	port.NetworkID = "some-other-network"
	if _, err := FromPort(port, testSubnets(), testNetwork(), Options{DefaultOVSBridge: "br-int"}); err == nil {
		t.Fatal("expected an error when the port is not on the given network")
	}
}

func TestFromPortRejectsUnknownSubnet(t *testing.T) {
	port := testPort(nil)
	port.FixedIPs = []openstack.FixedIP{{SubnetID: "not-requested", IPAddress: "10.9.9.9"}}
	_, err := FromPort(port, testSubnets(), testNetwork(), Options{DefaultOVSBridge: "br-int"})
	if err == nil {
		t.Fatal("expected an error when the port has no address on any requested subnet")
	}
}

func TestBuildSubnetsIsDeterministic(t *testing.T) {
	// Two addresses on one subnet plus a second subnet: the grouping must
	// preserve fixed-IP order so repeated conversions do not churn the CRD.
	const sub2 = "11112222-3333-4444-5555-666677778888"
	subnets := testSubnets()
	subnets[sub2] = &openstack.Subnet{
		ID: sub2, NetworkID: testNetID, CIDR: "fd00::/64", GatewayIP: "fd00::1", IPVersion: 6,
	}

	port := testPort(nil)
	port.FixedIPs = []openstack.FixedIP{
		{SubnetID: testSubID, IPAddress: "10.0.1.42"},
		{SubnetID: sub2, IPAddress: "fd00::42"},
		{SubnetID: testSubID, IPAddress: "10.0.1.43"},
	}

	for i := 0; i < 5; i++ {
		v, err := FromPort(port, subnets, testNetwork(), Options{DefaultOVSBridge: "br-int"})
		if err != nil {
			t.Fatalf("FromPort: %v", err)
		}
		if len(v.Network.Subnets) != 2 {
			t.Fatalf("got %d subnets, want 2", len(v.Network.Subnets))
		}
		if v.Network.Subnets[0].ID != testSubID {
			t.Errorf("first subnet = %q, want %q", v.Network.Subnets[0].ID, testSubID)
		}
		if got := v.Network.Subnets[0].IPs; len(got) != 2 || got[0] != "10.0.1.42" || got[1] != "10.0.1.43" {
			t.Errorf("subnet 0 IPs = %v, want [10.0.1.42 10.0.1.43]", got)
		}
	}
}

func TestFixedIPsForIsSorted(t *testing.T) {
	set := SubnetSet{
		"ccc": {ID: "ccc", NetworkID: testNetID},
		"aaa": {ID: "aaa", NetworkID: testNetID},
		"bbb": {ID: "bbb", NetworkID: testNetID},
	}
	got := FixedIPsFor(set)
	want := []string{"aaa", "bbb", "ccc"}
	if len(got) != len(want) {
		t.Fatalf("got %d fixed IPs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].SubnetID != want[i] {
			t.Errorf("fixed IP %d = %q, want %q", i, got[i].SubnetID, want[i])
		}
	}
}

func TestSubnetSetNetworkID(t *testing.T) {
	id, ok := testSubnets().NetworkID()
	if !ok || id != testNetID {
		t.Errorf("NetworkID() = %q,%v; want %q,true", id, ok, testNetID)
	}

	mixed := testSubnets()
	mixed["other"] = &openstack.Subnet{ID: "other", NetworkID: "different-net"}
	if _, ok := mixed.NetworkID(); ok {
		t.Error("expected NetworkID to report inconsistency across networks")
	}
}
