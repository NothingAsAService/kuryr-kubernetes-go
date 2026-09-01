package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/policy"
)

func newNetworkPolicy(mods ...func(*netv1.NetworkPolicy)) *netv1.NetworkPolicy {
	np := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-web",
			Namespace: "default",
			UID:       types.UID("np-uid-1"),
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeIngress},
			Ingress: []netv1.NetworkPolicyIngressRule{{
				From: []netv1.NetworkPolicyPeer{{
					IPBlock: &netv1.IPBlock{CIDR: "10.0.0.0/8"},
				}},
			}},
		},
	}
	for _, m := range mods {
		m(np)
	}
	return np
}

func newPolicyReconciler(t *testing.T, objs ...client.Object) *NetworkPolicyReconciler {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &NetworkPolicyReconciler{
		Client:   c,
		Scheme:   scheme,
		Config:   config.Defaults(),
		Resolver: &policy.ClientResolver{Client: c},
	}
}

func TestNetworkPolicyCreatesKuryrNetworkPolicy(t *testing.T) {
	np := newNetworkPolicy()
	r := newPolicyReconciler(t, np)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: np.Namespace, Name: np.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var knp kuryrv1.KuryrNetworkPolicy
	if err := r.Get(context.Background(),
		types.NamespacedName{Namespace: np.Namespace, Name: np.Name}, &knp); err != nil {
		t.Fatalf("reading the KuryrNetworkPolicy: %v", err)
	}

	// The finalizer must be there from the first write, or a delete arriving
	// before the security group is recorded would leak it.
	if !controllerutil.ContainsFinalizer(&knp, kuryrv1.KuryrNetworkPolicyFinalizer) {
		t.Error("KuryrNetworkPolicy has no finalizer")
	}
	if len(knp.OwnerReferences) != 1 || knp.OwnerReferences[0].Name != np.Name {
		t.Errorf("ownerReferences = %v, want the NetworkPolicy", knp.OwnerReferences)
	}
	if len(knp.Spec.IngressRules) != 1 {
		t.Fatalf("got %d ingress rules, want 1", len(knp.Spec.IngressRules))
	}
	if got := knp.Spec.IngressRules[0].Rule.RemoteIPPrefix; got != "10.0.0.0/8" {
		t.Errorf("remote prefix = %q, want 10.0.0.0/8", got)
	}
	// An ingress-only policy contributes no egress rules. Egress is left open
	// per pod by the security groups driver, which reads policyTypes, so that
	// this policy cannot also open egress for a pod another policy confines.
	if len(knp.Spec.EgressRules) != 0 {
		t.Errorf("got %d egress rules, want none from an ingress-only policy", len(knp.Spec.EgressRules))
	}
	if len(knp.Spec.PolicyTypes) != 1 || knp.Spec.PolicyTypes[0] != "Ingress" {
		t.Errorf("policyTypes = %v, want [Ingress]", knp.Spec.PolicyTypes)
	}
}

func TestNetworkPolicyReconcileIsIdempotent(t *testing.T) {
	// A second pass over unchanged input must not rewrite the object, or the
	// controller wakes itself up forever.
	np := newNetworkPolicy()
	r := newPolicyReconciler(t, np)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: np.Namespace, Name: np.Name}}

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile pass %d: %v", i, err)
		}
	}

	var knp kuryrv1.KuryrNetworkPolicy
	if err := r.Get(context.Background(), req.NamespacedName, &knp); err != nil {
		t.Fatalf("reading the KuryrNetworkPolicy: %v", err)
	}
	if knp.Generation > 1 {
		t.Errorf("generation = %d, want 1: the second pass rewrote an unchanged object",
			knp.Generation)
	}
}

func TestNetworkPolicyReconcileUpdatesRules(t *testing.T) {
	np := newNetworkPolicy()
	r := newPolicyReconciler(t, np)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: np.Namespace, Name: np.Name}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var current netv1.NetworkPolicy
	if err := r.Get(context.Background(), req.NamespacedName, &current); err != nil {
		t.Fatalf("reading the NetworkPolicy: %v", err)
	}
	current.Spec.Ingress[0].From[0].IPBlock.CIDR = "192.168.0.0/16"
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("updating the NetworkPolicy: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile after update: %v", err)
	}

	var knp kuryrv1.KuryrNetworkPolicy
	if err := r.Get(context.Background(), req.NamespacedName, &knp); err != nil {
		t.Fatalf("reading the KuryrNetworkPolicy: %v", err)
	}
	if got := knp.Spec.IngressRules[0].Rule.RemoteIPPrefix; got != "192.168.0.0/16" {
		t.Errorf("remote prefix = %q, want the updated 192.168.0.0/16", got)
	}
}

func TestNetworkPolicyReconcileIgnoresMissingPolicy(t *testing.T) {
	// A deleted NetworkPolicy takes its KuryrNetworkPolicy with it through
	// the owner reference, so there is nothing to do and nothing to fail on.
	r := newPolicyReconciler(t)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "gone"},
	}); err != nil {
		t.Fatalf("Reconcile on a missing policy: %v", err)
	}
}

func TestPoliciesForPodMatchesTargetAndPeer(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"env": "prod"}},
	}

	// Selects the pod as its target.
	target := newNetworkPolicy(func(np *netv1.NetworkPolicy) {
		np.Name = "targets-web"
	})
	// Selects the pod as a peer, from another namespace.
	peer := newNetworkPolicy(func(np *netv1.NetworkPolicy) {
		np.Name = "allows-from-web"
		np.Namespace = "other"
		np.Spec.PodSelector = metav1.LabelSelector{}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"env": "prod"},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
			}},
		}}
	})
	// Mentions neither.
	unrelated := newNetworkPolicy(func(np *netv1.NetworkPolicy) {
		np.Name = "unrelated"
		np.Spec.PodSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{"app": "database"},
		}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{
				IPBlock: &netv1.IPBlock{CIDR: "172.16.0.0/12"},
			}},
		}}
	})

	pod := newPod(func(p *corev1.Pod) {
		p.Labels = map[string]string{"app": "web"}
		p.Status.PodIP = "10.0.1.5"
	})

	r := newPolicyReconciler(t, ns, target, peer, unrelated, pod)

	got := map[string]bool{}
	for _, req := range r.policiesForPod(context.Background(), pod) {
		got[req.Name] = true
	}

	if !got["targets-web"] {
		t.Error("the policy selecting the pod was not enqueued")
	}
	if !got["allows-from-web"] {
		t.Error("the cross-namespace policy referencing the pod was not enqueued")
	}
	if got["unrelated"] {
		t.Error("a policy that never mentions the pod was enqueued")
	}
}

func TestPoliciesForPodSkipsHostNetwork(t *testing.T) {
	// A host-network pod uses the node's address and never appears in a rule.
	pod := newPod(func(p *corev1.Pod) {
		p.Spec.HostNetwork = true
		p.Labels = map[string]string{"app": "web"}
	})
	r := newPolicyReconciler(t, newNetworkPolicy(), pod)

	if reqs := r.policiesForPod(context.Background(), pod); len(reqs) != 0 {
		t.Errorf("got %v, want nothing for a host-network pod", reqs)
	}
}

func TestPoliciesForNamespaceEnqueuesSelectorPolicies(t *testing.T) {
	local := newNetworkPolicy(func(np *netv1.NetworkPolicy) { np.Name = "local" })
	crossNamespace := newNetworkPolicy(func(np *netv1.NetworkPolicy) {
		np.Name = "cross"
		np.Namespace = "other"
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{},
			}},
		}}
	})
	elsewhere := newNetworkPolicy(func(np *netv1.NetworkPolicy) {
		np.Name = "elsewhere"
		np.Namespace = "other"
	})

	r := newPolicyReconciler(t, local, crossNamespace, elsewhere)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}

	got := map[string]bool{}
	for _, req := range r.policiesForNamespace(context.Background(), ns) {
		got[req.Name] = true
	}

	if !got["local"] {
		t.Error("a policy in the namespace was not enqueued")
	}
	if !got["cross"] {
		t.Error("a policy with a namespaceSelector was not enqueued")
	}
	if got["elsewhere"] {
		t.Error("an unrelated policy in another namespace was enqueued")
	}
}
