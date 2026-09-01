package controller

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
)

// NamespaceReconciler declares that a namespace needs its own network.
//
// It writes a KuryrNetwork and nothing else; building the Neutron resources
// is KuryrNetworkReconciler's job. Keeping the two apart means a Neutron
// outage produces retries against KuryrNetwork objects rather than against
// Namespaces, which every other controller in the cluster also watches.
type NamespaceReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Config  *config.Config
	Project drivers.ProjectDriver
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrnetworks,verbs=get;list;watch;create;update;patch;delete

// Reconcile makes sure a namespace that needs a network has a KuryrNetwork.
func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			// The namespace is gone. Kubernetes garbage collects the
			// KuryrNetwork inside it, and that object's own finalizer
			// releases the Neutron resources.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading namespace %s: %w", req.Name, err)
	}

	if !ns.DeletionTimestamp.IsZero() {
		// Nothing to create in a namespace that is going away.
		return ctrl.Result{}, nil
	}

	var kn kuryrv1.KuryrNetwork
	key := types.NamespacedName{Namespace: ns.Name, Name: ns.Name}
	err := r.Get(ctx, key, &kn)

	switch {
	case err == nil:
		return r.syncLabels(ctx, &ns, &kn)

	case apierrors.IsNotFound(err):
		needed, err := r.needsNetwork(ctx, ns.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !needed {
			// No pod here will ever use a Neutron port. Creating a network
			// and subnet anyway would spend quota on nothing, which matters
			// on a cluster with many system namespaces.
			logger.V(1).Info("namespace has no pods needing a Neutron port, skipping")
			return ctrl.Result{}, nil
		}
		return r.create(ctx, &ns)

	default:
		return ctrl.Result{}, fmt.Errorf("reading KuryrNetwork for namespace %s: %w", ns.Name, err)
	}
}

// needsNetwork reports whether any pod in the namespace uses pod networking.
func (r *NamespaceReconciler) needsNetwork(ctx context.Context, namespace string) (bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return false, fmt.Errorf("listing pods in namespace %s: %w", namespace, err)
	}
	for i := range pods.Items {
		if !pods.Items[i].Spec.HostNetwork {
			return true, nil
		}
	}
	return false, nil
}

// create writes the KuryrNetwork for a namespace.
func (r *NamespaceReconciler) create(ctx context.Context, ns *corev1.Namespace) (ctrl.Result, error) {
	projectID, err := r.Project.ProjectFor(ctx, ns)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving project for namespace %s: %w", ns.Name, err)
	}

	kn := &kuryrv1.KuryrNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:       ns.Name,
			Namespace:  ns.Name,
			Finalizers: []string{kuryrv1.KuryrNetworkFinalizer},
		},
		Spec: kuryrv1.KuryrNetworkSpec{
			NamespaceName:   ns.Name,
			ProjectID:       projectID,
			NamespaceLabels: maps.Clone(ns.Labels),
		},
	}

	if err := r.Create(ctx, kn); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{RequeueAfter: recheckRequeue}, nil
		}
		if apierrors.IsForbidden(err) {
			// The namespace started terminating between our read and this
			// write. Expected, and it resolves itself.
			log.FromContext(ctx).V(1).Info("namespace is terminating, skipping KuryrNetwork creation")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("creating KuryrNetwork for namespace %s: %w", ns.Name, err)
	}

	log.FromContext(ctx).Info("created KuryrNetwork", "project", projectID)
	return ctrl.Result{}, nil
}

// syncLabels copies namespace labels onto the KuryrNetwork.
//
// NetworkPolicy rules can select namespaces by label, so a label change has
// to reach the policy controller. Recording the labels here is what turns a
// namespace edit into a KuryrNetwork event it can act on.
func (r *NamespaceReconciler) syncLabels(ctx context.Context, ns *corev1.Namespace,
	kn *kuryrv1.KuryrNetwork) (ctrl.Result, error) {

	if maps.Equal(kn.Spec.NamespaceLabels, ns.Labels) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(kn.DeepCopy())
	kn.Spec.NamespaceLabels = maps.Clone(ns.Labels)
	if err := r.Patch(ctx, kn, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("updating labels on KuryrNetwork %s: %w", kn.Name, err)
	}

	log.FromContext(ctx).V(1).Info("namespace labels changed, KuryrNetwork updated")
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler.
//
// Pods are watched as well, mapped to their namespace. That is what makes the
// "only create a network once something needs it" rule work: the first pod in
// a namespace wakes this controller. The Python implementation achieved the
// same thing by having the pod handler write a random annotation onto the
// namespace to force an event, which is a lot of machinery for one edge.
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podToNamespace)).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.Config.Kubernetes.MaxConcurrentReconciles,
		}).
		Named("namespace").
		Complete(r)
}

// podToNamespace maps a pod event onto its namespace.
func podToNamespace(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.HostNetwork {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: pod.Namespace},
	}}
}
