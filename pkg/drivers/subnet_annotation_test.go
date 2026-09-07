package drivers

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

const (
	tenantProject = "project-tenant-a"
	otherProject  = "project-tenant-b"
)

// fixedSubnetDriver stands in for whatever the cluster does when nothing is
// annotated, so the tests can tell "fell back" from "honoured the annotation".
type fixedSubnetDriver struct {
	subnet *openstack.Subnet
	calls  int
	err    error
}

func (f *fixedSubnetDriver) SubnetsFor(context.Context, client.Object, string) (vif.SubnetSet, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return vif.SubnetSet{f.subnet.ID: f.subnet}, nil
}

func newAnnotationSubnets(t *testing.T, base SubnetDriver, enforceProject bool,
	objs ...client.Object) *annotationSubnetDriver {

	t.Helper()
	c := fake.NewClientBuilder().WithScheme(policyTestScheme(t)).WithObjects(objs...).Build()

	return &annotationSubnetDriver{
		k8s:  c,
		base: base,
		os: fakeSubnets{byID: map[string]*openstack.Subnet{
			"subnet-tenant": {
				ID: "subnet-tenant", NetworkID: "net-1",
				CIDR: "10.20.0.0/24", ProjectID: tenantProject,
			},
			"subnet-foreign": {
				ID: "subnet-foreign", NetworkID: "net-2",
				CIDR: "10.30.0.0/24", ProjectID: otherProject,
			},
		}},
		enforceProject: enforceProject,
	}
}

// tenantNamespace is the namespace every test pod lives in, standing for one
// tenant's slice of the cluster.
const tenantNamespace = "tenant-a"

func podWithAnnotations(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-0",
			Namespace:   tenantNamespace,
			Annotations: annotations,
		},
	}
}

func namespaceWithAnnotations(annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: tenantNamespace, Annotations: annotations},
	}
}

func onlySubnetID(t *testing.T, got vif.SubnetSet) string {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("got %d subnets, want exactly 1: %v", len(got), got)
	}
	for id := range got {
		return id
	}
	return ""
}

func TestAnnotationSubnetHonoursThePodAnnotation(t *testing.T) {
	base := &fixedSubnetDriver{subnet: &openstack.Subnet{ID: "subnet-default"}}
	d := newAnnotationSubnets(t, base, true,
		namespaceWithAnnotations(nil))

	pod := podWithAnnotations(map[string]string{SubnetAnnotation: "subnet-tenant"})
	got, err := d.SubnetsFor(context.Background(), pod, tenantProject)
	if err != nil {
		t.Fatalf("SubnetsFor: %v", err)
	}
	if id := onlySubnetID(t, got); id != "subnet-tenant" {
		t.Errorf("subnet = %s, want subnet-tenant", id)
	}
	if base.calls != 0 {
		t.Errorf("the fallback driver was consulted %d times, want 0", base.calls)
	}
}

func TestAnnotationSubnetFallsBackWhenNothingIsAnnotated(t *testing.T) {
	base := &fixedSubnetDriver{subnet: &openstack.Subnet{ID: "subnet-default"}}
	d := newAnnotationSubnets(t, base, true, namespaceWithAnnotations(nil))

	got, err := d.SubnetsFor(context.Background(), podWithAnnotations(nil), tenantProject)
	if err != nil {
		t.Fatalf("SubnetsFor: %v", err)
	}
	if id := onlySubnetID(t, got); id != "subnet-default" {
		t.Errorf("subnet = %s, want the fallback driver's subnet-default", id)
	}
	if base.calls != 1 {
		t.Errorf("the fallback driver was consulted %d times, want 1", base.calls)
	}
}

// A tenant that wants every pod in a namespace on one subnet should be able
// to say so once, rather than on every pod spec.
func TestAnnotationSubnetInheritsFromTheNamespace(t *testing.T) {
	base := &fixedSubnetDriver{subnet: &openstack.Subnet{ID: "subnet-default"}}
	d := newAnnotationSubnets(t, base, true,
		namespaceWithAnnotations(map[string]string{SubnetAnnotation: "subnet-tenant"}))

	got, err := d.SubnetsFor(context.Background(), podWithAnnotations(nil), tenantProject)
	if err != nil {
		t.Fatalf("SubnetsFor: %v", err)
	}
	if id := onlySubnetID(t, got); id != "subnet-tenant" {
		t.Errorf("subnet = %s, want subnet-tenant from the namespace annotation", id)
	}
	if base.calls != 0 {
		t.Errorf("the fallback driver was consulted %d times, want 0", base.calls)
	}
}

func TestAnnotationSubnetPodAnnotationBeatsTheNamespace(t *testing.T) {
	base := &fixedSubnetDriver{subnet: &openstack.Subnet{ID: "subnet-default"}}
	d := newAnnotationSubnets(t, base, false,
		namespaceWithAnnotations(map[string]string{SubnetAnnotation: "subnet-foreign"}))

	pod := podWithAnnotations(map[string]string{SubnetAnnotation: "subnet-tenant"})
	got, err := d.SubnetsFor(context.Background(), pod, tenantProject)
	if err != nil {
		t.Fatalf("SubnetsFor: %v", err)
	}
	if id := onlySubnetID(t, got); id != "subnet-tenant" {
		t.Errorf("subnet = %s, want the pod annotation to win", id)
	}
}

// This is the tenancy boundary: without it, any user who can annotate a pod
// could put a port on another tenant's subnet.
func TestAnnotationSubnetRefusesAnotherProjectsSubnet(t *testing.T) {
	base := &fixedSubnetDriver{subnet: &openstack.Subnet{ID: "subnet-default"}}
	d := newAnnotationSubnets(t, base, true, namespaceWithAnnotations(nil))

	pod := podWithAnnotations(map[string]string{SubnetAnnotation: "subnet-foreign"})
	_, err := d.SubnetsFor(context.Background(), pod, tenantProject)

	var notAllowed *ErrSubnetNotAllowed
	if !errors.As(err, &notAllowed) {
		t.Fatalf("SubnetsFor error = %v, want ErrSubnetNotAllowed", err)
	}
	if notAllowed.SubnetID != "subnet-foreign" || notAllowed.PodProject != tenantProject {
		t.Errorf("error = %+v, want it to name subnet-foreign and %s", notAllowed, tenantProject)
	}
	// Retrying cannot make a cross-tenant subnet allowed, so the caller must
	// not treat this as a transient failure.
	if errors.Is(err, ErrRetryLater) {
		t.Error("a cross-project subnet must not be reported as retryable")
	}
}

func TestAnnotationSubnetAllowsCrossProjectWhenEnforcementIsOff(t *testing.T) {
	base := &fixedSubnetDriver{subnet: &openstack.Subnet{ID: "subnet-default"}}
	d := newAnnotationSubnets(t, base, false, namespaceWithAnnotations(nil))

	pod := podWithAnnotations(map[string]string{SubnetAnnotation: "subnet-foreign"})
	got, err := d.SubnetsFor(context.Background(), pod, tenantProject)
	if err != nil {
		t.Fatalf("SubnetsFor: %v", err)
	}
	if id := onlySubnetID(t, got); id != "subnet-foreign" {
		t.Errorf("subnet = %s, want subnet-foreign", id)
	}
}

// A typo in the annotation must name the annotation in the error: the ID came
// from a user, and they are the one who has to fix it.
func TestAnnotationSubnetReportsAnUnknownSubnet(t *testing.T) {
	base := &fixedSubnetDriver{subnet: &openstack.Subnet{ID: "subnet-default"}}
	d := newAnnotationSubnets(t, base, true, namespaceWithAnnotations(nil))

	pod := podWithAnnotations(map[string]string{SubnetAnnotation: "subnet-typo"})
	_, err := d.SubnetsFor(context.Background(), pod, tenantProject)
	if err == nil {
		t.Fatal("expected an error for a subnet that does not exist")
	}
	for _, want := range []string{SubnetAnnotation, "subnet-typo", tenantNamespace + "/web-0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if base.calls != 0 {
		t.Errorf("the fallback driver was consulted %d times, want 0: a bad annotation "+
			"must not silently put the pod on the default subnet", base.calls)
	}
}
