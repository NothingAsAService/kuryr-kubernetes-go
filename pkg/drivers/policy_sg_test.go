package drivers

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

func policyTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := kuryrv1.AddToScheme(s); err != nil {
		t.Fatalf("adding kuryr scheme: %v", err)
	}
	return s
}

// knpFor builds a policy governing both directions, which is the case where
// a pod's groups are exactly the matching policies' own groups. Tests about
// the direction nothing restricts use knpGoverning instead.
func knpFor(name string, selector map[string]string, sgID string) *kuryrv1.KuryrNetworkPolicy {
	return knpGoverning(name, selector, sgID, "Ingress", "Egress")
}

func knpGoverning(name string, selector map[string]string, sgID string,
	policyTypes ...string) *kuryrv1.KuryrNetworkPolicy {

	return &kuryrv1.KuryrNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: kuryrv1.KuryrNetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selector},
			PolicyTypes: policyTypes,
		},
		Status: kuryrv1.KuryrNetworkPolicyStatus{SecurityGroupID: sgID},
	}
}

func webPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-0",
			Namespace: "default",
			Labels:    map[string]string{"app": "web", "tier": "front"},
		},
	}
}

func policySGDriverForTest(t *testing.T, objs ...client.Object) SecurityGroupsDriver {
	t.Helper()
	drv, _ := policySGDriverWithCloud(t, objs...)
	return drv
}

// policySGDriverWithCloud also hands back the fake Neutron, so a test can see
// which shared groups the driver reached for.
//
// The struct is built here rather than through newPolicySGDriver because the
// constructor insists on a real *openstack.Client, and the only thing the
// driver needs from it is the handful of calls fakeAllowAllCloud implements.
func policySGDriverWithCloud(t *testing.T, objs ...client.Object) (*policySGDriver, *fakeAllowAllCloud) {
	t.Helper()
	cfg := config.Defaults()
	cfg.NeutronDefaults.PodSecurityGroups = []string{"default-sg"}

	c := fake.NewClientBuilder().WithScheme(policyTestScheme(t)).WithObjects(objs...).Build()
	cloud := newFakeAllowAllCloud()
	return &policySGDriver{
		k8s:      c,
		defaults: cfg.NeutronDefaults.PodSecurityGroups,
		allowAll: newAllowAllGroups(cloud),
	}, cloud
}

func TestPolicySGUnselectedPodGetsDefaults(t *testing.T) {
	// A pod no policy selects is unrestricted, which is what the configured
	// default group expresses.
	drv := policySGDriverForTest(t, knpFor("other", map[string]string{"app": "database"}, "sg-db"))

	got, err := drv.SecurityGroupsFor(context.Background(), webPod(), "project-1")
	if err != nil {
		t.Fatalf("SecurityGroupsFor: %v", err)
	}
	if len(got) != 1 || got[0] != "default-sg" {
		t.Errorf("groups = %v, want the configured default", got)
	}
}

func TestPolicySGSelectedPodGetsPolicyGroups(t *testing.T) {
	drv := policySGDriverForTest(t,
		knpFor("allow-web", map[string]string{"app": "web"}, "sg-web"),
		knpFor("allow-front", map[string]string{"tier": "front"}, "sg-front"),
		knpFor("unrelated", map[string]string{"app": "database"}, "sg-db"),
	)

	got, err := drv.SecurityGroupsFor(context.Background(), webPod(), "project-1")
	if err != nil {
		t.Fatalf("SecurityGroupsFor: %v", err)
	}

	// Sorted, so the port update is a no-op when nothing actually changed.
	want := []string{"sg-front", "sg-web"}
	if len(got) != len(want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("groups = %v, want %v", got, want)
		}
	}
}

func TestPolicySGWaitsForAnUncreatedGroup(t *testing.T) {
	// Falling back to the defaults here would leave the pod unrestricted for
	// as long as the security group took to appear.
	drv := policySGDriverForTest(t, knpFor("allow-web", map[string]string{"app": "web"}, ""))

	_, err := drv.SecurityGroupsFor(context.Background(), webPod(), "project-1")
	if !errors.Is(err, ErrRetryLater) {
		t.Fatalf("error = %v, want a retry-later error", err)
	}
}

func TestPolicySGIgnoresPolicyBeingDeleted(t *testing.T) {
	deleting := knpFor("allow-web", map[string]string{"app": "web"}, "sg-web")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{kuryrv1.KuryrNetworkPolicyFinalizer}

	drv := policySGDriverForTest(t, deleting)

	got, err := drv.SecurityGroupsFor(context.Background(), webPod(), "project-1")
	if err != nil {
		t.Fatalf("SecurityGroupsFor: %v", err)
	}
	if len(got) != 1 || got[0] != "default-sg" {
		t.Errorf("groups = %v, want the defaults once the policy is going away", got)
	}
}

func TestPolicySGRequiresDefaults(t *testing.T) {
	// Without defaults a pod selected by no policy would get no group at all,
	// which Neutron reads as the project default. Fail at startup instead.
	cfg := config.Defaults()
	cfg.NeutronDefaults.PodSecurityGroups = nil
	c := fake.NewClientBuilder().WithScheme(policyTestScheme(t)).Build()

	_, err := newPolicySGDriver(Deps{Config: cfg, K8s: c, OpenStack: &openstack.Client{}})
	if err == nil {
		t.Fatal("expected an error when pod_security_groups is unset")
	}
	if !strings.Contains(err.Error(), "pod_security_groups") {
		t.Errorf("error = %v, want it to name the missing setting", err)
	}
}

// The three tests below are the fix for a long-standing Kuryr bug.
//
// Upstream puts allow-everything rules in the group of any policy that does
// not govern a direction. Because Neutron unions a port's groups, a single
// permissive policy then reopens that direction for every pod it selects --
// including pods that a second, restrictive policy was meant to isolate.
// Kubernetes decides isolation per pod, so the decision has to be made with
// all of a pod's policies in view.

func TestPolicySGOpensTheDirectionNoPolicyRestricts(t *testing.T) {
	// One ingress-only policy: egress is governed by nothing, so it has to
	// stay open, and the shared group is what expresses that.
	drv, cloud := policySGDriverWithCloud(t,
		knpGoverning("allow-web", map[string]string{"app": "web"}, "sg-web", "Ingress"))

	got, err := drv.SecurityGroupsFor(context.Background(), webPod(), "project-1")
	if err != nil {
		t.Fatalf("SecurityGroupsFor: %v", err)
	}

	egress, err := cloud.FindSecurityGroupsByName(context.Background(),
		allowAllEgressGroupName, "project-1")
	if err != nil || len(egress) != 1 {
		t.Fatalf("the shared egress group was not created: %v / %v", egress, err)
	}
	if !contains(got, "sg-web") || !contains(got, egress[0].ID) {
		t.Fatalf("groups = %v, want sg-web plus the shared egress group %s", got, egress[0].ID)
	}
	ingress, err := cloud.FindSecurityGroupsByName(context.Background(),
		allowAllIngressGroupName, "project-1")
	if err != nil {
		t.Fatalf("FindSecurityGroupsByName: %v", err)
	}
	if len(ingress) != 0 {
		t.Error("an ingress allow-all group was created even though a policy restricts ingress")
	}
}

func TestPolicySGDoesNotWidenARestrictedDirection(t *testing.T) {
	// The bug itself: an egress-only policy and an ingress-only policy on the
	// same pod. Neither direction may be opened, because between them the two
	// policies restrict both.
	drv, cloud := policySGDriverWithCloud(t,
		knpGoverning("lock-ingress", map[string]string{"app": "web"}, "sg-in", "Ingress"),
		knpGoverning("lock-egress", map[string]string{"tier": "front"}, "sg-out", "Egress"),
	)

	got, err := drv.SecurityGroupsFor(context.Background(), webPod(), "project-1")
	if err != nil {
		t.Fatalf("SecurityGroupsFor: %v", err)
	}

	want := []string{"sg-in", "sg-out"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("groups = %v, want exactly %v with no allow-all group", got, want)
	}
	if cloud.creates != 0 {
		t.Errorf("created %d shared groups, want none: both directions are restricted", cloud.creates)
	}
}

func TestPolicySGOpensBothDirectionsWhenNeitherIsGoverned(t *testing.T) {
	// A policy listing no policyTypes at all is read as governing ingress,
	// the same default Kubernetes applies, so only egress opens.
	drv, cloud := policySGDriverWithCloud(t,
		knpGoverning("empty-types", map[string]string{"app": "web"}, "sg-web"))

	if _, err := drv.SecurityGroupsFor(context.Background(), webPod(), "project-1"); err != nil {
		t.Fatalf("SecurityGroupsFor: %v", err)
	}
	if cloud.creates != 1 {
		t.Fatalf("created %d shared groups, want 1 (egress only)", cloud.creates)
	}
	egress, err := cloud.FindSecurityGroupsByName(context.Background(),
		allowAllEgressGroupName, "project-1")
	if err != nil || len(egress) != 1 {
		t.Fatalf("the one group created should be the egress one: %v / %v", egress, err)
	}
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func TestPolicySGRejectsNonPods(t *testing.T) {
	drv := policySGDriverForTest(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

	if _, err := drv.SecurityGroupsFor(context.Background(), ns, "project-1"); err == nil {
		t.Fatal("expected an error for an object that is not a Pod")
	}
}
