package controller

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/policy"
)

// NetworkPolicyReconciler translates NetworkPolicy objects into
// KuryrNetworkPolicy objects.
//
// It touches no cloud at all: its whole job is to answer "what rules does
// this policy mean, right now, given the pods and namespaces in the cluster".
// KuryrNetworkPolicyReconciler then makes Neutron match. Splitting it this
// way means a selector change and a Neutron outage retry independently, and
// the translated form is visible in the API for anyone debugging a policy
// that is not doing what they expected.
type NetworkPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Config   *config.Config
	Resolver policy.Resolver
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrnetworkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile keeps a KuryrNetworkPolicy in step with its NetworkPolicy.
func (r *NetworkPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var np netv1.NetworkPolicy
	if err := r.Get(ctx, req.NamespacedName, &np); err != nil {
		if apierrors.IsNotFound(err) {
			// The KuryrNetworkPolicy is owned by the NetworkPolicy, so
			// Kubernetes deletes it, and its own finalizer releases the
			// security group. Nothing to do here.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading NetworkPolicy %s: %w", req.NamespacedName, err)
	}

	if !np.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	spec, err := policy.Translate(ctx, &np, r.Resolver, policy.Options{
		ServiceIngress: !r.Config.OctaviaDefaults.EnforceSGRules,
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	policy.SortRules(spec.IngressRules)
	policy.SortRules(spec.EgressRules)

	knp := &kuryrv1.KuryrNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      np.Name,
			Namespace: np.Namespace,
		},
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, knp, func() error {
		// The finalizer has to be present from the very first write: the
		// security group may be created before we are ever called again, and
		// a delete arriving in between would leak it.
		controllerutil.AddFinalizer(knp, kuryrv1.KuryrNetworkPolicyFinalizer)
		if err := controllerutil.SetControllerReference(&np, knp, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference: %w", err)
		}
		knp.Spec = spec
		return nil
	})
	if err != nil {
		if apierrors.IsConflict(err) {
			// Someone else wrote the object between our read and write.
			// Reconcile again on fresh data rather than forcing ours over it.
			return ctrl.Result{RequeueAfter: recheckRequeue}, nil
		}
		return ctrl.Result{}, fmt.Errorf("writing KuryrNetworkPolicy %s/%s: %w", np.Namespace, np.Name, err)
	}

	if result != controllerutil.OperationResultNone {
		logger.Info("KuryrNetworkPolicy written",
			"result", result,
			"ingressRules", len(spec.IngressRules),
			"egressRules", len(spec.EgressRules))
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler.
//
// A policy's rules depend on more than the policy itself: pod addresses and
// labels, namespace labels, and whether a namespace has its own subnet yet.
// Each of those is watched and mapped back to the policies it can change.
func (r *NetworkPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1.NetworkPolicy{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.policiesForPod)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.policiesForNamespace)).
		Watches(&kuryrv1.KuryrNetwork{}, handler.EnqueueRequestsFromMapFunc(r.policiesForKuryrNetwork)).
		// Egress rules include the cluster IPs of Services fronting allowed
		// peers, and hairpin ingress the namespace's cluster IPs. Python bumps
		// policies on Service create and delete for the same reason.
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.allPolicies),
			builder.WithPredicates(serviceAddressingChanged)).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.Config.Kubernetes.MaxConcurrentReconciles,
		}).
		Named("networkpolicy").
		Complete(r)
}

// policiesForPod returns the policies whose rules can change because a pod
// did.
//
// This replaces the Python pod label handler. Rather than a separate
// controller diffing labels, a pod event is mapped onto the policies that
// actually reference it, and the ordinary reconcile recomputes them. Fewer
// moving parts, and it also covers a pod gaining an address, which a label
// diff would miss.
func (r *NetworkPolicyReconciler) policiesForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	pod, ok := obj.(*corev1.Pod)
	if !ok {
		logger.Error(nil, "pod watch produced a non-pod object", "type", reflect.TypeOf(obj))
		return nil
	}
	if pod.Spec.HostNetwork {
		// A host-network pod never appears in a rule.
		return nil
	}

	// Namespace labels decide whether a cross-namespace peer reaches this
	// pod. Reading them from the cache costs no API call.
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: pod.Namespace}, &ns); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "reading namespace to map a pod onto policies", "namespace", pod.Namespace)
		}
		// Without namespace labels we cannot tell which cross-namespace
		// policies match, so fall back to the policies in the pod's own
		// namespace rather than dropping the event.
		return r.policiesIn(ctx, pod.Namespace)
	}

	var list netv1.NetworkPolicyList
	if err := r.List(ctx, &list); err != nil {
		logger.Error(err, "listing NetworkPolicies to map a pod")
		return nil
	}

	var out []reconcile.Request
	for i := range list.Items {
		np := &list.Items[i]
		if policyReferencesPod(np, pod, ns.Labels) {
			out = append(out, requestFor(np))
		}
	}
	return out
}

// policiesForNamespace returns the policies whose rules can change because a
// namespace's labels did.
func (r *NetworkPolicyReconciler) policiesForNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		logger.Error(nil, "namespace watch produced a non-namespace object", "type", reflect.TypeOf(obj))
		return nil
	}

	var list netv1.NetworkPolicyList
	if err := r.List(ctx, &list); err != nil {
		logger.Error(err, "listing NetworkPolicies to map a namespace")
		return nil
	}

	var out []reconcile.Request
	for i := range list.Items {
		np := &list.Items[i]
		// A policy in the namespace always depends on it, and any policy
		// with a namespaceSelector might now match or stop matching it.
		if np.Namespace == ns.Name || policyHasNamespaceSelector(np) {
			out = append(out, requestFor(np))
		}
	}
	return out
}

// policiesForKuryrNetwork returns the policies affected by a namespace subnet
// appearing or changing.
//
// Until a namespace's subnet exists, a namespaceSelector peer is expanded to
// individual pod addresses. Once it exists the same peer collapses to a
// single rule, so the policies have to be recomputed.
func (r *NetworkPolicyReconciler) policiesForKuryrNetwork(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	kn, ok := obj.(*kuryrv1.KuryrNetwork)
	if !ok {
		logger.Error(nil, "KuryrNetwork watch produced an unexpected object", "type", reflect.TypeOf(obj))
		return nil
	}

	var list netv1.NetworkPolicyList
	if err := r.List(ctx, &list); err != nil {
		logger.Error(err, "listing NetworkPolicies to map a KuryrNetwork")
		return nil
	}

	var out []reconcile.Request
	for i := range list.Items {
		np := &list.Items[i]
		if np.Namespace == kn.Spec.NamespaceName || policyHasNamespaceSelector(np) {
			out = append(out, requestFor(np))
		}
	}
	return out
}

// policiesIn returns every policy in one namespace.
func (r *NetworkPolicyReconciler) policiesIn(ctx context.Context, namespace string) []reconcile.Request {
	var list netv1.NetworkPolicyList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		log.FromContext(ctx).Error(err, "listing NetworkPolicies in a namespace", "namespace", namespace)
		return nil
	}

	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, requestFor(&list.Items[i]))
	}
	return out
}

// allPolicies returns every policy in the cluster. A Service can be the
// destination of a peer in any namespace, so its changes are not confined to
// the policies of its own namespace.
func (r *NetworkPolicyReconciler) allPolicies(ctx context.Context, _ client.Object) []reconcile.Request {
	return r.policiesIn(ctx, metav1.NamespaceAll)
}

// serviceAddressingChanged passes Service events that can change a rule: a
// Service appearing or going, or its cluster IPs or selector changing.
var serviceAddressingChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldSvc, okOld := e.ObjectOld.(*corev1.Service)
		newSvc, okNew := e.ObjectNew.(*corev1.Service)
		if !okOld || !okNew {
			return true
		}
		return !slices.Equal(oldSvc.Spec.ClusterIPs, newSvc.Spec.ClusterIPs) ||
			!maps.Equal(oldSvc.Spec.Selector, newSvc.Spec.Selector) ||
			oldSvc.DeletionTimestamp.IsZero() != newSvc.DeletionTimestamp.IsZero()
	},
}

func requestFor(np *netv1.NetworkPolicy) reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: np.Namespace, Name: np.Name},
	}
}

// policyReferencesPod reports whether a pod can appear in a policy's
// translation, either as a target or as a peer.
func policyReferencesPod(np *netv1.NetworkPolicy, pod *corev1.Pod, nsLabels map[string]string) bool {
	// The pod is a target of the policy.
	if np.Namespace == pod.Namespace && selectorMatches(&np.Spec.PodSelector, pod.Labels) {
		return true
	}

	for _, rule := range np.Spec.Ingress {
		if peersReferencePod(rule.From, np.Namespace, pod, nsLabels) {
			return true
		}
	}
	for _, rule := range np.Spec.Egress {
		if peersReferencePod(rule.To, np.Namespace, pod, nsLabels) {
			return true
		}
	}
	return false
}

func peersReferencePod(peers []netv1.NetworkPolicyPeer, policyNamespace string,
	pod *corev1.Pod, nsLabels map[string]string) bool {

	for _, peer := range peers {
		if peer.IPBlock != nil {
			// An ipBlock names addresses directly and never depends on pods.
			continue
		}

		// Decide whether the pod's namespace is in the peer's scope.
		if peer.NamespaceSelector == nil {
			if pod.Namespace != policyNamespace {
				continue
			}
		} else if !selectorMatches(peer.NamespaceSelector, nsLabels) {
			continue
		}

		// A peer with no podSelector covers every pod in scope.
		if peer.PodSelector == nil || selectorMatches(peer.PodSelector, pod.Labels) {
			return true
		}
	}
	return false
}

func policyHasNamespaceSelector(np *netv1.NetworkPolicy) bool {
	for _, rule := range np.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.NamespaceSelector != nil {
				return true
			}
		}
	}
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil {
				return true
			}
		}
	}
	return false
}

// selectorMatches reports whether a label set satisfies a selector.
//
// A selector that does not parse matches nothing. The API server validates
// selectors on admission, so this only happens on a malformed object, and
// treating it as "matches nothing" is the safe reading: it drops the pod from
// a rule rather than silently widening one.
func selectorMatches(sel *metav1.LabelSelector, l map[string]string) bool {
	if sel == nil {
		return true
	}
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(l))
}
