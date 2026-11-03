package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

// fakeSGCloud records what the reconciler asks Neutron to do.
type fakeSGCloud struct {
	mu sync.Mutex

	groups map[string]*openstack.SecurityGroup
	rules  map[string]openstack.SGRule
	next   int

	// Injected failures, each firing once.
	createGroupErr error
	createRuleErr  error
	deleteGroupErr error
	deleteRuleErr  error
}

func newFakeSGCloud() *fakeSGCloud {
	return &fakeSGCloud{
		groups: map[string]*openstack.SecurityGroup{},
		rules:  map[string]openstack.SGRule{},
	}
}

func (f *fakeSGCloud) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s-%d", prefix, f.next)
}

func (f *fakeSGCloud) CreateSecurityGroup(_ context.Context, name, projectID, description string) (*openstack.SecurityGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createGroupErr; err != nil {
		f.createGroupErr = nil
		return nil, err
	}
	sg := &openstack.SecurityGroup{
		ID:          f.id("sg"),
		Name:        name,
		ProjectID:   projectID,
		Description: description,
	}
	f.groups[sg.ID] = sg
	return sg, nil
}

func (f *fakeSGCloud) FindSecurityGroupsByName(_ context.Context, name, _ string) ([]*openstack.SecurityGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*openstack.SecurityGroup
	for _, sg := range f.groups {
		if sg.Name == name {
			out = append(out, sg)
		}
	}
	return out, nil
}

func (f *fakeSGCloud) DeleteSecurityGroup(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.deleteGroupErr; err != nil {
		f.deleteGroupErr = nil
		return err
	}
	delete(f.groups, id)
	for ruleID, r := range f.rules {
		if r.SecurityGroupID == id {
			delete(f.rules, ruleID)
		}
	}
	return nil
}

func (f *fakeSGCloud) CreateSGRule(_ context.Context, r openstack.SGRule) (*openstack.SGRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createRuleErr; err != nil {
		f.createRuleErr = nil
		return nil, err
	}
	r.ID = f.id("rule")
	f.rules[r.ID] = r
	return &r, nil
}

func (f *fakeSGCloud) DeleteSGRule(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.deleteRuleErr; err != nil {
		f.deleteRuleErr = nil
		return err
	}
	delete(f.rules, id)
	return nil
}

func (f *fakeSGCloud) GetSubnet(_ context.Context, id string) (*openstack.Subnet, error) {
	return nil, fmt.Errorf("no subnet %s in this test", id)
}

func (f *fakeSGCloud) ruleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rules)
}

func (f *fakeSGCloud) groupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.groups)
}

func ingressRule(cidr string, port int32) kuryrv1.SGRuleSpec {
	return kuryrv1.SGRuleSpec{
		Rule: kuryrv1.SGRule{
			Direction:      kuryrv1.SGDirectionIngress,
			EtherType:      kuryrv1.SGEtherTypeIPv4,
			Protocol:       "tcp",
			PortRangeMin:   port,
			PortRangeMax:   port,
			RemoteIPPrefix: cidr,
		},
	}
}

func newKNP(mods ...func(*kuryrv1.KuryrNetworkPolicy)) *kuryrv1.KuryrNetworkPolicy {
	knp := &kuryrv1.KuryrNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "allow-web",
			Namespace:  "default",
			UID:        types.UID("knp-uid-1"),
			Finalizers: []string{kuryrv1.KuryrNetworkPolicyFinalizer},
		},
		Spec: kuryrv1.KuryrNetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			PolicyTypes:  []string{"Ingress"},
			IngressRules: []kuryrv1.SGRuleSpec{ingressRule("10.0.0.0/8", 80)},
		},
	}
	for _, m := range mods {
		m(knp)
	}
	return knp
}

func newKNPReconciler(t *testing.T, cloud securityGroupCloud, objs ...client.Object) *KuryrNetworkPolicyReconciler {
	t.Helper()
	scheme := testScheme(t)
	cfg := config.Defaults()
	cfg.NeutronDefaults.Project = "project-1"
	// The base rules need real subnets, which this fake has none of. They are
	// covered by their own configuration rather than every test.
	cfg.PodVIFNested.WorkerNodesSubnets = nil
	cfg.OctaviaDefaults.EnforceSGRules = false
	cfg.NeutronDefaults.ServiceSubnet = ""

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&kuryrv1.KuryrNetworkPolicy{}).
		Build()

	return &KuryrNetworkPolicyReconciler{
		Client:    c,
		Scheme:    scheme,
		Config:    cfg,
		OpenStack: cloud,
	}
}

func reconcileKNP(t *testing.T, r *KuryrNetworkPolicyReconciler, knp *kuryrv1.KuryrNetworkPolicy) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: knp.Namespace, Name: knp.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return result
}

func readKNP(t *testing.T, r *KuryrNetworkPolicyReconciler, knp *kuryrv1.KuryrNetworkPolicy) *kuryrv1.KuryrNetworkPolicy {
	t.Helper()
	var out kuryrv1.KuryrNetworkPolicy
	key := types.NamespacedName{Namespace: knp.Namespace, Name: knp.Name}
	if err := r.Get(context.Background(), key, &out); err != nil {
		t.Fatalf("reading the KuryrNetworkPolicy: %v", err)
	}
	return &out
}

func TestKNPCreatesGroupAndRules(t *testing.T) {
	cloud := newFakeSGCloud()
	knp := newKNP()
	r := newKNPReconciler(t, cloud, knp)

	reconcileKNP(t, r, knp)

	got := readKNP(t, r, knp)
	if got.Status.SecurityGroupID == "" {
		t.Fatal("no security group was recorded")
	}
	if len(got.Status.SecurityGroupRules) != 1 {
		t.Fatalf("got %d recorded rules, want 1", len(got.Status.SecurityGroupRules))
	}
	if cloud.ruleCount() != 1 {
		t.Errorf("Neutron has %d rules, want 1", cloud.ruleCount())
	}
	cond := findCondition(got, kuryrv1.KuryrNetworkPolicyConditionEnforced)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Enforced condition = %v, want True", cond)
	}
}

func TestKNPSecondPassCreatesNothingNew(t *testing.T) {
	// Rules are matched by content, so an unchanged policy must not churn
	// Neutron. Rewriting a live group's rules would drop traffic.
	cloud := newFakeSGCloud()
	knp := newKNP()
	r := newKNPReconciler(t, cloud, knp)

	reconcileKNP(t, r, knp)
	firstIDs := map[string]bool{}
	for _, cr := range readKNP(t, r, knp).Status.SecurityGroupRules {
		firstIDs[cr.ID] = true
	}

	reconcileKNP(t, r, knp)

	if cloud.ruleCount() != 1 {
		t.Errorf("Neutron has %d rules after a no-op pass, want 1", cloud.ruleCount())
	}
	if cloud.groupCount() != 1 {
		t.Errorf("Neutron has %d groups after a no-op pass, want 1", cloud.groupCount())
	}
	for _, cr := range readKNP(t, r, knp).Status.SecurityGroupRules {
		if !firstIDs[cr.ID] {
			t.Errorf("rule %s was recreated on a pass that changed nothing", cr.ID)
		}
	}
}

func TestKNPAddsAndRemovesRules(t *testing.T) {
	cloud := newFakeSGCloud()
	knp := newKNP()
	r := newKNPReconciler(t, cloud, knp)

	reconcileKNP(t, r, knp)

	// Swap one rule for two others.
	current := readKNP(t, r, knp)
	current.Spec.IngressRules = []kuryrv1.SGRuleSpec{
		ingressRule("192.168.0.0/16", 443),
		ingressRule("172.16.0.0/12", 443),
	}
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatalf("updating the spec: %v", err)
	}

	reconcileKNP(t, r, knp)

	got := readKNP(t, r, knp)
	if len(got.Status.SecurityGroupRules) != 2 {
		t.Fatalf("got %d recorded rules, want 2", len(got.Status.SecurityGroupRules))
	}
	if cloud.ruleCount() != 2 {
		t.Errorf("Neutron has %d rules, want 2: the old rule was not removed", cloud.ruleCount())
	}
	for _, cr := range got.Status.SecurityGroupRules {
		if cr.Rule.RemoteIPPrefix == "10.0.0.0/8" {
			t.Error("the removed rule is still recorded")
		}
	}
}

func TestKNPDeduplicatesIdenticalRules(t *testing.T) {
	// Two peers can expand to the same security group rule. Creating it twice
	// would make Neutron reject the second as a conflict every pass.
	cloud := newFakeSGCloud()
	knp := newKNP(func(knp *kuryrv1.KuryrNetworkPolicy) {
		knp.Spec.IngressRules = []kuryrv1.SGRuleSpec{
			ingressRule("10.0.0.0/8", 80),
			ingressRule("10.0.0.0/8", 80),
		}
	})
	r := newKNPReconciler(t, cloud, knp)

	reconcileKNP(t, r, knp)

	if cloud.ruleCount() != 1 {
		t.Errorf("Neutron has %d rules, want 1 for two identical specs", cloud.ruleCount())
	}
}

func TestKNPAdoptsAnExistingGroup(t *testing.T) {
	// A pass that created the group and then failed to record it must not
	// leak a second group on the retry.
	cloud := newFakeSGCloud()
	knp := newKNP()
	r := newKNPReconciler(t, cloud, knp)

	name := policySecurityGroupName(knp.Namespace, knp.Name, string(knp.UID))
	orphan, err := cloud.CreateSecurityGroup(context.Background(), name, "project-1", "left over")
	if err != nil {
		t.Fatalf("seeding the orphaned group: %v", err)
	}

	reconcileKNP(t, r, knp)

	if got := readKNP(t, r, knp).Status.SecurityGroupID; got != orphan.ID {
		t.Errorf("security group = %q, want the adopted %q", got, orphan.ID)
	}
	if cloud.groupCount() != 1 {
		t.Errorf("Neutron has %d groups, want 1: a duplicate was created", cloud.groupCount())
	}
}

func TestKNPRecordsRulesCreatedBeforeAFailure(t *testing.T) {
	// When rule creation fails partway, what was created must still be
	// recorded, or the retry loses track of it and leaks.
	cloud := newFakeSGCloud()
	knp := newKNP(func(knp *kuryrv1.KuryrNetworkPolicy) {
		knp.Spec.IngressRules = []kuryrv1.SGRuleSpec{
			ingressRule("10.0.0.0/8", 80),
			ingressRule("10.0.0.0/8", 443),
		}
	})
	r := newKNPReconciler(t, cloud, knp)

	// Let the group and the first rule succeed, then fail the second.
	cloud.createRuleErr = nil
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: knp.Namespace, Name: knp.Name},
	}); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	// Now drop everything from Neutron and make the next create fail, as if
	// the cloud had gone away mid-sync.
	current := readKNP(t, r, knp)
	current.Spec.IngressRules = append(current.Spec.IngressRules, ingressRule("172.16.0.0/12", 8080))
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatalf("updating the spec: %v", err)
	}
	boom := errors.New("neutron is down")
	cloud.createRuleErr = boom

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: knp.Namespace, Name: knp.Name},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Reconcile error = %v, want the injected failure", err)
	}

	got := readKNP(t, r, knp)
	if len(got.Status.SecurityGroupRules) != 2 {
		t.Errorf("got %d recorded rules, want the 2 that were created before the failure",
			len(got.Status.SecurityGroupRules))
	}
	cond := findCondition(got, kuryrv1.KuryrNetworkPolicyConditionEnforced)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Enforced condition = %v, want False after a failure", cond)
	}
}

func TestKNPKeepsEveryRuleThatSurvivesAFailedDelete(t *testing.T) {
	// One failed delete must not stop the others, and every rule still in
	// Neutron must stay in the status, or nothing ever deletes it.
	cloud := newFakeSGCloud()
	knp := newKNP(func(knp *kuryrv1.KuryrNetworkPolicy) {
		knp.Spec.IngressRules = []kuryrv1.SGRuleSpec{
			ingressRule("10.0.0.0/8", 80),
			ingressRule("10.0.0.0/8", 443),
			ingressRule("10.0.0.0/8", 8080),
		}
	})
	r := newKNPReconciler(t, cloud, knp)
	reconcileKNP(t, r, knp)

	current := readKNP(t, r, knp)
	current.Spec.IngressRules = nil
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatalf("updating the spec: %v", err)
	}
	boom := errors.New("neutron is down")
	cloud.deleteRuleErr = boom

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: knp.Namespace, Name: knp.Name},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Reconcile error = %v, want the injected failure", err)
	}

	if cloud.ruleCount() != 1 {
		t.Errorf("Neutron has %d rules, want 1: the other deletes were not attempted", cloud.ruleCount())
	}
	got := readKNP(t, r, knp)
	if len(got.Status.SecurityGroupRules) != cloud.ruleCount() {
		t.Fatalf("status records %d rules, Neutron has %d: a surviving rule was lost",
			len(got.Status.SecurityGroupRules), cloud.ruleCount())
	}
	if _, ok := cloud.rules[got.Status.SecurityGroupRules[0].ID]; !ok {
		t.Error("the recorded rule is not the one still in Neutron")
	}

	// The retry deletes what is left.
	reconcileKNP(t, r, knp)
	if cloud.ruleCount() != 0 || len(readKNP(t, r, knp).Status.SecurityGroupRules) != 0 {
		t.Errorf("after the retry: Neutron has %d rules, want 0", cloud.ruleCount())
	}
}

func TestKNPFinalizeDeletesTheGroup(t *testing.T) {
	cloud := newFakeSGCloud()
	knp := newKNP()
	r := newKNPReconciler(t, cloud, knp)

	reconcileKNP(t, r, knp)

	current := readKNP(t, r, knp)
	if err := r.Delete(context.Background(), current); err != nil {
		t.Fatalf("deleting the KuryrNetworkPolicy: %v", err)
	}

	reconcileKNP(t, r, knp)

	if cloud.groupCount() != 0 {
		t.Errorf("Neutron still has %d groups after finalizing", cloud.groupCount())
	}
	var out kuryrv1.KuryrNetworkPolicy
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: knp.Namespace, Name: knp.Name}, &out)
	if err == nil {
		t.Errorf("the KuryrNetworkPolicy still exists with finalizers %v", out.Finalizers)
	}
}

func TestRuleKeyIgnoresDescription(t *testing.T) {
	// The description carries provenance, not behaviour. Including it would
	// recreate every rule in Neutron when a policy is merely renamed.
	a := kuryrv1.SGRule{
		Direction: kuryrv1.SGDirectionIngress, EtherType: kuryrv1.SGEtherTypeIPv4,
		Protocol: "tcp", PortRangeMin: 80, PortRangeMax: 80,
		RemoteIPPrefix: "10.0.0.0/8", Description: "one",
	}
	b := a
	b.Description = "another"

	if ruleKey(a) != ruleKey(b) {
		t.Errorf("keys differ on description alone:\n%q\n%q", ruleKey(a), ruleKey(b))
	}

	c := a
	c.PortRangeMax = 443
	if ruleKey(a) == ruleKey(c) {
		t.Error("keys match despite a different port range")
	}
}

// findCondition returns a condition by type, or nil.
func findCondition(knp *kuryrv1.KuryrNetworkPolicy, condType string) *metav1.Condition {
	for i := range knp.Status.Conditions {
		if knp.Status.Conditions[i].Type == condType {
			return &knp.Status.Conditions[i]
		}
	}
	return nil
}

func TestKNPReportsGroupCreationFailure(t *testing.T) {
	cloud := newFakeSGCloud()
	boom := errors.New("quota exceeded")
	cloud.createGroupErr = boom

	knp := newKNP()
	r := newKNPReconciler(t, cloud, knp)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: knp.Namespace, Name: knp.Name},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Reconcile error = %v, want the injected failure", err)
	}

	got := readKNP(t, r, knp)
	if got.Status.SecurityGroupID != "" {
		t.Errorf("security group = %q, want none recorded after a failure", got.Status.SecurityGroupID)
	}
	cond := findCondition(got, kuryrv1.KuryrNetworkPolicyConditionEnforced)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Enforced condition = %v, want False", cond)
	}
	if cond.Message == "" {
		t.Error("the condition says nothing about why the policy is not enforced")
	}
}

func TestKNPKeepsFinalizerWhenDeletionFails(t *testing.T) {
	// Dropping the finalizer on a failed delete would strand the security
	// group in Neutron with nothing left pointing at it.
	cloud := newFakeSGCloud()
	knp := newKNP()
	r := newKNPReconciler(t, cloud, knp)

	reconcileKNP(t, r, knp)

	current := readKNP(t, r, knp)
	if err := r.Delete(context.Background(), current); err != nil {
		t.Fatalf("deleting the KuryrNetworkPolicy: %v", err)
	}

	boom := errors.New("neutron is down")
	cloud.deleteGroupErr = boom

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: knp.Namespace, Name: knp.Name},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Reconcile error = %v, want the injected failure", err)
	}

	got := readKNP(t, r, knp)
	if !controllerutil.ContainsFinalizer(got, kuryrv1.KuryrNetworkPolicyFinalizer) {
		t.Error("the finalizer was removed even though the group could not be deleted")
	}
	if cloud.groupCount() != 1 {
		t.Errorf("Neutron has %d groups, want the undeleted 1", cloud.groupCount())
	}
}
