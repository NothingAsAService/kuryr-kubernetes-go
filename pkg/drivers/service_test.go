package drivers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
)

func webService(mods ...func(*corev1.Service)) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "web"}},
	}
	for _, m := range mods {
		m(svc)
	}
	return svc
}

func backendPod(name string, labels map[string]string, mods ...func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Status:     corev1.PodStatus{PodIP: "10.0.1.5"},
	}
	for _, m := range mods {
		m(pod)
	}
	return pod
}

func policyServiceSGForTest(t *testing.T, objs ...client.Object) SecurityGroupsDriver {
	t.Helper()
	cfg := config.Defaults()
	cfg.NeutronDefaults.PodSecurityGroups = []string{"default-sg"}

	c := fake.NewClientBuilder().WithScheme(policyTestScheme(t)).WithObjects(objs...).Build()
	// Built directly, for the same reason as policySGDriverWithCloud: the
	// constructor wants a real *openstack.Client and the driver only needs
	// the few calls the fake implements.
	return &policyServiceSGDriver{
		k8s: c,
		pods: &policySGDriver{
			k8s:      c,
			defaults: cfg.NeutronDefaults.PodSecurityGroups,
			allowAll: newAllowAllGroups(newFakeAllowAllCloud()),
		},
		defaults: cfg.NeutronDefaults.PodSecurityGroups,
	}
}

func TestPolicyServiceSGInheritsFromBackends(t *testing.T) {
	// A Service is never selected by a NetworkPolicy itself; its load
	// balancer has to carry whatever its backends carry, or the VIP would be
	// reachable when the pods behind it are not.
	drv := policyServiceSGForTest(t,
		knpFor("allow-web", map[string]string{"app": "web"}, "sg-web"),
		backendPod("web-0", map[string]string{"app": "web"}),
	)

	got, err := drv.SecurityGroupsFor(context.Background(), webService(), "project-1")
	if err != nil {
		t.Fatalf("SecurityGroupsFor: %v", err)
	}
	if len(got) != 1 || got[0] != "sg-web" {
		t.Errorf("groups = %v, want the backend's policy group", got)
	}
}

func TestPolicyServiceSGWithoutSelectorUsesDefaults(t *testing.T) {
	// NetworkPolicy does not reach a Service with no selector, because there
	// is no backend to inherit a policy from.
	drv := policyServiceSGForTest(t,
		knpFor("allow-web", map[string]string{"app": "web"}, "sg-web"),
	)
	svc := webService(func(s *corev1.Service) { s.Spec.Selector = nil })

	got, err := drv.SecurityGroupsFor(context.Background(), svc, "project-1")
	if err != nil {
		t.Fatalf("SecurityGroupsFor: %v", err)
	}
	if len(got) != 1 || got[0] != "default-sg" {
		t.Errorf("groups = %v, want the defaults", got)
	}
}

func TestPolicyServiceSGWithNoBackendsYetUsesDefaults(t *testing.T) {
	drv := policyServiceSGForTest(t,
		knpFor("allow-web", map[string]string{"app": "web"}, "sg-web"),
	)

	got, err := drv.SecurityGroupsFor(context.Background(), webService(), "project-1")
	if err != nil {
		t.Fatalf("SecurityGroupsFor: %v", err)
	}
	if len(got) != 1 || got[0] != "default-sg" {
		t.Errorf("groups = %v, want the defaults until a backend appears", got)
	}
}

func TestPolicyServiceSGSkipsHostNetworkBackends(t *testing.T) {
	// A host-network backend has no Neutron port, so it carries no policy
	// group to inherit.
	drv := policyServiceSGForTest(t,
		knpFor("allow-web", map[string]string{"app": "web"}, "sg-web"),
		backendPod("web-0", map[string]string{"app": "web"}, func(p *corev1.Pod) {
			p.Spec.HostNetwork = true
		}),
	)

	got, err := drv.SecurityGroupsFor(context.Background(), webService(), "project-1")
	if err != nil {
		t.Fatalf("SecurityGroupsFor: %v", err)
	}
	if len(got) != 1 || got[0] != "default-sg" {
		t.Errorf("groups = %v, want the defaults", got)
	}
}

func TestPolicyServiceSGRejectsNonServices(t *testing.T) {
	drv := policyServiceSGForTest(t)
	pod := backendPod("web-0", map[string]string{"app": "web"})

	if _, err := drv.SecurityGroupsFor(context.Background(), pod, "project-1"); err == nil {
		t.Fatal("expected an error for an object that is not a Service")
	}
}

func TestDefaultServiceSubnetDriverRequiresConfiguration(t *testing.T) {
	// Without a service subnet the load balancer VIP has nowhere to live, and
	// finding that out per Service would be much harder to diagnose.
	cfg := config.Defaults()
	cfg.NeutronDefaults.ServiceSubnet = ""

	if _, err := newDefaultServiceSubnetDriver(Deps{Config: cfg}); err == nil {
		t.Fatal("expected an error when service_subnet is unset")
	}
}
