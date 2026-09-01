package drivers

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

func TestParseNetworksJSONForm(t *testing.T) {
	got, err := parseNetworks(`[{"name":"net-a"},{"name":"net-b","namespace":"other"}]`)
	if err != nil {
		t.Fatalf("parseNetworks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d networks, want 2", len(got))
	}
	if got[0].Name != "net-a" || got[0].Namespace != "" {
		t.Errorf("first network = %+v, want net-a with no namespace", got[0])
	}
	if got[1].Name != "net-b" || got[1].Namespace != "other" {
		t.Errorf("second network = %+v, want other/net-b", got[1])
	}
}

func TestParseNetworksShorthandForm(t *testing.T) {
	// The specification also allows a comma-separated list, and real
	// manifests use it, so both have to work.
	got, err := parseNetworks("net-a, other/net-b")
	if err != nil {
		t.Fatalf("parseNetworks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d networks, want 2", len(got))
	}
	if got[0].Name != "net-a" || got[0].Namespace != "" {
		t.Errorf("first network = %+v, want net-a", got[0])
	}
	if got[1].Namespace != "other" || got[1].Name != "net-b" {
		t.Errorf("second network = %+v, want other/net-b", got[1])
	}
}

func TestParseNetworksEmpty(t *testing.T) {
	for _, annotation := range []string{"", "   "} {
		got, err := parseNetworks(annotation)
		if err != nil {
			t.Fatalf("parseNetworks(%q): %v", annotation, err)
		}
		if len(got) != 0 {
			t.Errorf("parseNetworks(%q) = %v, want nothing", annotation, got)
		}
	}
}

func TestParseNetworksRejectsGarbage(t *testing.T) {
	if _, err := parseNetworks(`[{"namespace":"other"}]`); err == nil {
		t.Error("expected an error for a network entry with no name")
	}
	if _, err := parseNetworks(`[{"name":`); err == nil {
		t.Error("expected an error for malformed JSON")
	}
	if _, err := parseNetworks("a/b/c"); err == nil {
		t.Error("expected an error for an entry that is neither a name nor namespace/name")
	}
}

// fakeSubnets answers subnet lookups from fixed data.
type fakeSubnets struct{ byID map[string]*openstack.Subnet }

func (f fakeSubnets) GetSubnet(_ context.Context, id string) (*openstack.Subnet, error) {
	s, ok := f.byID[id]
	if !ok {
		return nil, fmt.Errorf("no subnet %s", id)
	}
	return s, nil
}

// recordingVIFDriver hands back a port and remembers what it was asked for.
type recordingVIFDriver struct {
	name     string
	requests []PodVIFRequest
	err      error
	next     int
}

func (d *recordingVIFDriver) Name() string { return d.name }

func (d *recordingVIFDriver) RequestVIF(_ context.Context, req PodVIFRequest) (*kuryrv1.VIF, error) {
	d.requests = append(d.requests, req)
	if d.err != nil {
		return nil, d.err
	}
	d.next++
	return &kuryrv1.VIF{ID: fmt.Sprintf("%s-port-%d", d.name, d.next)}, nil
}

func (d *recordingVIFDriver) RequestVIFs(context.Context, PodVIFRequest, int) ([]*kuryrv1.VIF, error) {
	return nil, errors.New("not used by these tests")
}
func (d *recordingVIFDriver) ReleaseVIF(context.Context, *kuryrv1.VIF, PodVIFRequest) error {
	return nil
}
func (d *recordingVIFDriver) ActivateVIF(context.Context, *kuryrv1.VIF, PodVIFRequest) error {
	return nil
}
func (d *recordingVIFDriver) UpdateVIFSecurityGroups(context.Context, *kuryrv1.VIF, []string) error {
	return nil
}

func nad(namespace, name, kuryrConfig string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(nadGVK)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if kuryrConfig != "" {
		obj.SetAnnotations(map[string]string{KuryrConfigAnnotation: kuryrConfig})
	}
	return obj
}

func annotatedPod(networks string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-0",
			Namespace:   "default",
			Annotations: map[string]string{NetworksAnnotation: networks},
		},
	}
}

func newNPWG(t *testing.T, defaultDriver PodVIFDriver, objs ...client.Object) *npwgMultiVIF {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(policyTestScheme(t)).WithObjects(objs...).Build()

	return &npwgMultiVIF{
		k8s: c,
		os: fakeSubnets{byID: map[string]*openstack.Subnet{
			"subnet-extra":   {ID: "subnet-extra", CIDR: "10.5.0.0/24"},
			"subnet-default": {ID: "subnet-default", CIDR: "10.0.0.0/24"},
			"subnet-alias":   {ID: "subnet-alias", CIDR: "10.9.0.0/24"},
		}},
		cfg: subnetDefaults{
			PodSubnet: "subnet-default",
			PerDriver: map[string]string{"nested-vlan": "subnet-alias"},
		},
		resolve: func(alias string) (PodVIFDriver, error) {
			if alias == "" {
				return defaultDriver, nil
			}
			return nil, fmt.Errorf("no driver %q", alias)
		},
	}
}

func TestNPWGCreatesOnePortPerNetwork(t *testing.T) {
	drv := &recordingVIFDriver{name: "neutron-vif"}
	n := newNPWG(t, drv,
		nad("default", "net-a", `{"subnetId":"subnet-extra"}`),
		nad("default", "net-b", `{}`),
	)

	got, err := n.AdditionalVIFs(context.Background(), PodVIFRequest{
		Pod:       annotatedPod("net-a,net-b"),
		ProjectID: "project-1",
	})
	if err != nil {
		t.Fatalf("AdditionalVIFs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d extra ports, want one per network", len(got))
	}

	// The first network names its own subnet; the second falls back to the
	// default pod subnet.
	if _, ok := drv.requests[0].Subnets["subnet-extra"]; !ok {
		t.Errorf("first request used %v, want the subnet the NAD names",
			subnetIDs(drv.requests[0].Subnets))
	}
	if _, ok := drv.requests[1].Subnets["subnet-default"]; !ok {
		t.Errorf("second request used %v, want the default pod subnet",
			subnetIDs(drv.requests[1].Subnets))
	}
}

func TestNPWGWithoutAnnotationDoesNothing(t *testing.T) {
	drv := &recordingVIFDriver{name: "neutron-vif"}
	n := newNPWG(t, drv)

	got, err := n.AdditionalVIFs(context.Background(), PodVIFRequest{
		Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"}},
	})
	if err != nil {
		t.Fatalf("AdditionalVIFs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d extra ports, want none for a pod that asked for none", len(got))
	}
	if len(drv.requests) != 0 {
		t.Errorf("the VIF driver was called %d times for a pod with no networks",
			len(drv.requests))
	}
}

func TestNPWGFollowsTheDriverAliasSubnetMapping(t *testing.T) {
	// A NAD that names a driver but no subnet falls back to the subnet
	// mapped to that driver in kuryr.conf, not to the default pod subnet.
	aliasDriver := &recordingVIFDriver{name: "nested-vlan"}
	n := newNPWG(t, &recordingVIFDriver{name: "neutron-vif"},
		nad("default", "net-a", `{"driverType":"nested-vlan"}`))
	n.resolve = func(alias string) (PodVIFDriver, error) {
		if alias != "nested-vlan" {
			return nil, fmt.Errorf("unexpected alias %q", alias)
		}
		return aliasDriver, nil
	}

	if _, err := n.AdditionalVIFs(context.Background(), PodVIFRequest{
		Pod: annotatedPod("net-a"),
	}); err != nil {
		t.Fatalf("AdditionalVIFs: %v", err)
	}

	if len(aliasDriver.requests) != 1 {
		t.Fatalf("the aliased driver was called %d times, want 1", len(aliasDriver.requests))
	}
	if _, ok := aliasDriver.requests[0].Subnets["subnet-alias"]; !ok {
		t.Errorf("request used %v, want the subnet mapped to the driver alias",
			subnetIDs(aliasDriver.requests[0].Subnets))
	}
}

func TestNPWGReadsANetworkFromAnotherNamespace(t *testing.T) {
	drv := &recordingVIFDriver{name: "neutron-vif"}
	n := newNPWG(t, drv, nad("shared", "net-a", `{"subnetId":"subnet-extra"}`))

	got, err := n.AdditionalVIFs(context.Background(), PodVIFRequest{
		Pod: annotatedPod("shared/net-a"),
	})
	if err != nil {
		t.Fatalf("AdditionalVIFs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d extra ports, want 1", len(got))
	}
}

func TestNPWGReportsAMissingNetwork(t *testing.T) {
	// Silently skipping would give the pod fewer interfaces than it asked
	// for, which the workload would only discover at runtime.
	n := newNPWG(t, &recordingVIFDriver{name: "neutron-vif"})

	if _, err := n.AdditionalVIFs(context.Background(), PodVIFRequest{
		Pod: annotatedPod("net-a"),
	}); err == nil {
		t.Fatal("expected an error for a NetworkAttachmentDefinition that does not exist")
	}
}

func TestNPWGReportsANetworkWithoutKuryrConfig(t *testing.T) {
	// A NAD belonging to another CNI plugin has no Kuryr annotation, and
	// guessing a subnet for it would put the pod on the wrong network.
	n := newNPWG(t, &recordingVIFDriver{name: "neutron-vif"}, nad("default", "net-a", ""))

	if _, err := n.AdditionalVIFs(context.Background(), PodVIFRequest{
		Pod: annotatedPod("net-a"),
	}); err == nil {
		t.Fatal("expected an error for a NAD with no openstack.org/kuryr-config annotation")
	}
}

func TestNPWGReportsAnUnparseableKuryrConfig(t *testing.T) {
	n := newNPWG(t, &recordingVIFDriver{name: "neutron-vif"},
		nad("default", "net-a", `not json`))

	if _, err := n.AdditionalVIFs(context.Background(), PodVIFRequest{
		Pod: annotatedPod("net-a"),
	}); err == nil {
		t.Fatal("expected an error for an unparseable kuryr-config annotation")
	}
}

func TestNPWGReturnsPortsCreatedBeforeAFailure(t *testing.T) {
	// The caller releases whatever comes back. Dropping the ports created so
	// far would leak them.
	drv := &recordingVIFDriver{name: "neutron-vif"}
	n := newNPWG(t, drv, nad("default", "net-a", `{"subnetId":"subnet-extra"}`))

	got, err := n.AdditionalVIFs(context.Background(), PodVIFRequest{
		Pod: annotatedPod("net-a,net-missing"),
	})
	if err == nil {
		t.Fatal("expected an error for the missing second network")
	}
	if len(got) != 1 {
		t.Errorf("got %d ports back with the error, want the 1 already created", len(got))
	}
}

func TestNoopMultiVIFAddsNothing(t *testing.T) {
	got, err := noopMultiVIF{}.AdditionalVIFs(context.Background(), PodVIFRequest{
		Pod: annotatedPod("net-a"),
	})
	if err != nil {
		t.Fatalf("AdditionalVIFs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d extra ports from the noop driver, want none", len(got))
	}
}

func TestNewMultiVIFDriversRejectsAnUnknownName(t *testing.T) {
	_, err := NewMultiVIFDrivers([]string{"nonexistent"}, Deps{}, nil)
	if err == nil {
		t.Fatal("expected an error for an unknown multi-VIF driver")
	}
}

func subnetIDs(s vif.SubnetSet) []string {
	out := make([]string, 0, len(s))
	for id := range s {
		out = append(out, id)
	}
	return out
}
