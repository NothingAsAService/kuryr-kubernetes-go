package pool

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
)

func node(name, vifLabel string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if vifLabel != "" {
		n.Labels = map[string]string{NodeVIFLabel: vifLabel}
	}
	return n
}

func newTestMulti(t *testing.T, objs ...client.Object) (*Multi, map[string]*fakeVIFDriver) {
	t.Helper()

	cfg := config.Defaults()
	cfg.NeutronDefaults.Project = testProj
	cfg.NeutronDefaults.PodSubnet = testSubID
	cfg.Kubernetes.PodVIFDriver = "neutron-vif"
	cfg.VIFPool.PortsPoolMin = 2
	cfg.VIFPool.PortsPoolBatch = 2

	inner := map[string]*fakeVIFDriver{
		"neutron-vif": newFakeDriver("neutron-vif"),
		"nested-vlan": newFakeDriver("nested-vlan"),
	}
	asDrivers := map[string]drivers.PodVIFDriver{
		"neutron-vif": inner["neutron-vif"],
		"nested-vlan": inner["nested-vlan"],
	}

	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	m, err := NewMulti(cfg, newFakeCloud(), k8s, asDrivers, NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatalf("NewMulti: %v", err)
	}
	return m, inner
}

func TestMultiRoutesByNodeLabel(t *testing.T) {
	m, inner := newTestMulti(t,
		node("worker-1", "neutron-vif"),
		node("worker-2", "nested-vlan"),
	)
	ctx := context.Background()
	if err := m.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	baremetal := testRequest("sg-a")
	baremetal.Pod.Spec.NodeName = "worker-1"
	if _, err := m.RequestVIF(ctx, baremetal); err != nil {
		t.Fatalf("RequestVIF on the bare metal node: %v", err)
	}

	nested := testRequest("sg-a")
	nested.Pod.Spec.NodeName = "worker-2"
	if _, err := m.RequestVIF(ctx, nested); err != nil {
		t.Fatalf("RequestVIF on the nested node: %v", err)
	}

	if got := inner["neutron-vif"].createdCount(); got == 0 {
		t.Error("the bare metal node should have been served by the neutron-vif driver")
	}
	if got := inner["nested-vlan"].createdCount(); got == 0 {
		t.Error("the nested node should have been served by the nested-vlan driver")
	}
}

func TestMultiFallsBackToConfiguredDriver(t *testing.T) {
	// An unlabelled node uses [kubernetes]pod_vif_driver, which is what makes
	// the label optional on a homogeneous cluster.
	m, inner := newTestMulti(t, node("worker-1", ""))
	ctx := context.Background()
	_ = m.Recover(ctx)

	req := testRequest("sg-a")
	req.Pod.Spec.NodeName = "worker-1"
	if _, err := m.RequestVIF(ctx, req); err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}

	if inner["neutron-vif"].createdCount() == 0 {
		t.Error("an unlabelled node should fall back to the configured default driver")
	}
	if inner["nested-vlan"].createdCount() != 0 {
		t.Error("the nested driver should not have been used for an unlabelled node")
	}
}

func TestMultiRoutesReleaseByVIFType(t *testing.T) {
	// Release happens after the pod is gone, so the node label is no longer
	// reachable. The VIF type is what identifies the driver that made it.
	m, inner := newTestMulti(t, node("worker-2", "nested-vlan"))
	ctx := context.Background()
	_ = m.Recover(ctx)

	v := &kuryrv1.VIF{
		Type:    kuryrv1.VIFTypeNestedVLAN,
		ID:      "port-x",
		VLANID:  42,
		Network: kuryrv1.Network{ID: testNetID},
	}
	// With no pod and no host, the pool cannot place it, so it falls through
	// to the wrapped driver rather than being pooled.
	if err := m.ReleaseVIF(ctx, v, drivers.PodVIFRequest{}); err != nil {
		t.Fatalf("ReleaseVIF: %v", err)
	}

	released := inner["nested-vlan"].releasedIDs()
	if len(released) != 1 || released[0] != "port-x" {
		t.Errorf("nested driver released %v, want [port-x]", released)
	}
	if got := inner["neutron-vif"].releasedIDs(); len(got) != 0 {
		t.Errorf("the bare metal driver released %v; routing should have gone by VIF type", got)
	}
}

func TestMultiRejectsUnknownNodeLabel(t *testing.T) {
	m, _ := newTestMulti(t, node("worker-3", "nested-dpdk"))
	ctx := context.Background()
	_ = m.Recover(ctx)

	req := testRequest("sg-a")
	req.Pod.Spec.NodeName = "worker-3"

	_, err := m.RequestVIF(ctx, req)
	if err == nil {
		t.Fatal("expected an error for a node labelled with a driver that has no pool")
	}
}

func TestMultiRequiresDefaultDriverToHaveAPool(t *testing.T) {
	cfg := config.Defaults()
	cfg.NeutronDefaults.Project = testProj
	cfg.NeutronDefaults.PodSubnet = testSubID
	// The configured default is not among the drivers being pooled, so an
	// unlabelled node would have nowhere to go.
	cfg.Kubernetes.PodVIFDriver = "nested-macvlan"

	inner := map[string]drivers.PodVIFDriver{"neutron-vif": newFakeDriver("neutron-vif")}
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	if _, err := NewMulti(cfg, newFakeCloud(), k8s, inner, nil); err == nil {
		t.Fatal("expected NewMulti to reject a default driver with no pool")
	}
}

func TestAliasForVIFType(t *testing.T) {
	cases := map[kuryrv1.VIFType]string{
		kuryrv1.VIFTypeOVS:           "neutron-vif",
		kuryrv1.VIFTypeBridge:        "neutron-vif",
		kuryrv1.VIFTypeVHostUser:     "neutron-vif",
		kuryrv1.VIFTypeNestedVLAN:    "nested-vlan",
		kuryrv1.VIFTypeNestedMacvlan: "nested-macvlan",
		kuryrv1.VIFTypeNestedDPDK:    "nested-dpdk",
	}
	for vifType, want := range cases {
		got, ok := aliasForVIFType(vifType)
		if !ok || got != want {
			t.Errorf("aliasForVIFType(%q) = %q,%v; want %q,true", vifType, got, ok, want)
		}
	}
	if _, ok := aliasForVIFType("nonsense"); ok {
		t.Error("an unknown VIF type must not resolve to a driver")
	}
}
