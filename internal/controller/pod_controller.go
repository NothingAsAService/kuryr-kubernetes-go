// Package controller holds the reconcilers that keep OpenStack in step with
// Kubernetes.
//
// The Python implementation built its own event pipeline: a Watcher holding a
// long-poll per resource, an Async wrapper queueing by object UID, a Retry
// wrapper with exponential backoff, and a Dispatcher fanning out by kind.
// controller-runtime provides all four out of the box. An informer replaces
// the Watcher and gives a resync for free; the workqueue keys by object so
// events for one object are serialised while different objects proceed in
// parallel; the rate limiter replaces Retry; and registering a reconciler per
// type replaces the Dispatcher. What is left here is only the Kuryr logic.
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
)

// PodReconciler is the Kubernetes-facing half of pod networking. It creates a
// KuryrPort for every pod that needs one and nothing else.
//
// Splitting this from the OpenStack work is deliberate and inherited from the
// Python design: this reconciler only talks to the Kubernetes API, so it is
// fast and essentially cannot fail. Everything that can be slow or flaky
// happens in KuryrPortReconciler, keyed on the KuryrPort, which means a
// Neutron outage produces retries against KuryrPorts rather than a storm of
// updates against Pods.
type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *config.Config
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods/finalizers,verbs=update
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrports,verbs=get;list;watch;create;update;patch;delete

// Reconcile brings the KuryrPort for one pod into existence or removes it.
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		// The pod is gone and carried no finalizer, so there is nothing left
		// to clean up here; the KuryrPort owner reference handles the rest.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A host-network pod uses the node stack directly. Kuryr must not touch
	// it, and must not add a finalizer it would then have to remove.
	if pod.Spec.HostNetwork {
		return ctrl.Result{}, nil
	}

	if !pod.DeletionTimestamp.IsZero() || isPodCompleted(&pod) {
		return r.finalize(ctx, &pod)
	}

	// Until the scheduler has placed the pod there is no node to bind a port
	// to. The informer will bring us back when nodeName is set.
	if pod.Spec.NodeName == "" {
		return ctrl.Result{}, nil
	}

	// Take the finalizer before creating anything in OpenStack, so there is
	// no window in which a port exists but its owner can vanish.
	if !controllerutil.ContainsFinalizer(&pod, kuryrv1.PodFinalizer) {
		patch := client.MergeFrom(pod.DeepCopy())
		controllerutil.AddFinalizer(&pod, kuryrv1.PodFinalizer)
		if err := r.Patch(ctx, &pod, patch); err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: recheckRequeue}, nil
			}
			return ctrl.Result{}, fmt.Errorf("adding finalizer to pod %s: %w", req.NamespacedName, err)
		}
	}

	var kp kuryrv1.KuryrPort
	err := r.Get(ctx, req.NamespacedName, &kp)
	switch {
	case err == nil:
		// A KuryrPort with a stale UID belongs to a previous pod of the same
		// name, which happens with StatefulSets. Delete it and let its own
		// finalizer release the old port; we will recreate on the next pass.
		if kp.Spec.PodUID != pod.UID {
			logger.Info("removing KuryrPort left over from an earlier pod of the same name",
				"staleUID", kp.Spec.PodUID, "currentUID", pod.UID)
			if err := r.Delete(ctx, &kp); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("deleting stale KuryrPort: %w", err)
			}
			return ctrl.Result{RequeueAfter: recheckRequeue}, nil
		}
		return ctrl.Result{}, nil

	case apierrors.IsNotFound(err):
		return r.create(ctx, &pod)

	default:
		return ctrl.Result{}, fmt.Errorf("reading KuryrPort for pod %s: %w", req.NamespacedName, err)
	}
}

// create builds the KuryrPort for a scheduled pod.
func (r *PodReconciler) create(ctx context.Context, pod *corev1.Pod) (ctrl.Result, error) {
	kp := &kuryrv1.KuryrPort{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			// The node label is what lets each CNI daemon watch only the
			// KuryrPorts for its own node.
			Labels:     map[string]string{kuryrv1.NodeNameLabel: pod.Spec.NodeName},
			Finalizers: []string{kuryrv1.KuryrPortFinalizer},
		},
		Spec: kuryrv1.KuryrPortSpec{
			PodUID:      pod.UID,
			PodNodeName: pod.Spec.NodeName,
			PodStatic:   isStaticPod(pod),
		},
	}
	// An owner reference means the garbage collector removes the KuryrPort if
	// the pod is deleted while the controller is down.
	if err := controllerutil.SetControllerReference(pod, kp, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting owner reference on KuryrPort: %w", err)
	}

	if err := r.Create(ctx, kp); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{RequeueAfter: recheckRequeue}, nil
		}
		// A namespace being torn down rejects new objects. That is expected
		// and self-resolving, not an error worth retrying loudly.
		if apierrors.IsForbidden(err) {
			log.FromContext(ctx).Info("namespace is terminating, skipping KuryrPort creation")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("creating KuryrPort for pod %s/%s: %w",
			pod.Namespace, pod.Name, err)
	}

	log.FromContext(ctx).Info("created KuryrPort", "node", pod.Spec.NodeName)
	return ctrl.Result{}, nil
}

// finalize deletes the KuryrPort and waits for it to release its ports before
// letting the pod go.
func (r *PodReconciler) finalize(ctx context.Context, pod *corev1.Pod) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(pod, kuryrv1.PodFinalizer) {
		return ctrl.Result{}, nil
	}

	var kp kuryrv1.KuryrPort
	err := r.Get(ctx, client.ObjectKeyFromObject(pod), &kp)
	switch {
	case apierrors.IsNotFound(err):
		// Nothing left in OpenStack to wait for.

	case err != nil:
		return ctrl.Result{}, fmt.Errorf("reading KuryrPort while finalizing pod: %w", err)

	case kp.Spec.PodUID != pod.UID:
		// Belongs to a different incarnation; not ours to wait on.

	case kp.DeletionTimestamp.IsZero():
		if err := r.Delete(ctx, &kp); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting KuryrPort while finalizing pod: %w", err)
		}
		// The KuryrPort reconciler removes the pod finalizer once the ports
		// are actually released, so stop here rather than racing it.
		return ctrl.Result{}, nil

	default:
		// Deletion is already in flight; the KuryrPort reconciler will free
		// the pod when it is done.
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(pod.DeepCopy())
	controllerutil.RemoveFinalizer(pod, kuryrv1.PodFinalizer)
	if err := r.Patch(ctx, pod, patch); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("removing finalizer from pod: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Owns(&kuryrv1.KuryrPort{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.Config.Kubernetes.MaxConcurrentReconciles,
		}).
		Named("pod").
		Complete(r)
}

// isPodCompleted reports whether a pod has finished and will not run again,
// in which case its ports should be released even though the object remains.
func isPodCompleted(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return true
	default:
		return false
	}
}

// isStaticPod reports whether a pod is managed by kubelet from a manifest on
// disk rather than by the API server.
//
// This matters far downstream: a static pod has a mirror object in the API
// whose UID differs from the one the CRI reports to CNI, so the daemon has to
// skip its UID check for these. Recording it here saves the daemon a lookup
// on every ADD.
func isStaticPod(pod *corev1.Pod) bool {
	if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
		return true
	}
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "Node" {
			return true
		}
	}
	return false
}
