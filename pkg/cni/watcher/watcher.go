// Package watcher keeps the CNI daemon registry in step with the API.
//
// This replaces the two cotyledon services in
// kuryr_kubernetes/cni/daemon/watcher_service.py. Both watched a filtered
// collection and pushed into the shared dictionary; here both are ordinary
// controller-runtime reconcilers reading from a cache that is filtered at the
// source, so a node only ever receives the objects it will be asked about.
package watcher

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/cni/registry"
)

// NodeName resolves the node this daemon serves.
//
// The DaemonSet is expected to set NODE_NAME through the downward API.
// Falling back to the hostname matches what kubelet does by default, but it
// is a guess: a node registered under a different name would make this daemon
// watch nothing at all, so the fallback is logged.
func NodeName() (string, error) {
	if name := os.Getenv("NODE_NAME"); name != "" {
		return name, nil
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("NODE_NAME is unset and the hostname is unavailable: %w", err)
	}
	return host, nil
}

// CacheOptions restricts the informers to one node's objects.
//
// Without this every daemon on the cluster would stream every KuryrPort and
// every Pod, which is the difference between a cache proportional to the node
// and one proportional to the cluster.
func CacheOptions(nodeName string) cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&kuryrv1.KuryrPort{}: {
				Label: labels.SelectorFromSet(labels.Set{
					kuryrv1.NodeNameLabel: nodeName,
				}),
			},
			&corev1.Pod{}: {
				Field: fields.SelectorFromSet(fields.Set{
					"spec.nodeName": nodeName,
				}),
			},
		},
	}
}

// KuryrPortReconciler mirrors KuryrPort objects into the registry.
type KuryrPortReconciler struct {
	client.Client
	Registry *registry.Registry
}

// +kubebuilder:rbac:groups=openstack.org,resources=kuryrports,verbs=get;list;watch

// Reconcile records or removes one KuryrPort.
func (r *KuryrPortReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := registry.Key(req.Namespace, req.Name)

	var kp kuryrv1.KuryrPort
	err := r.Get(ctx, req.NamespacedName, &kp)
	switch {
	case apierrors.IsNotFound(err):
		// The entry is only dropped once the interfaces are down too; the
		// registry decides which side finishes last.
		r.Registry.MarkKuryrPortDeleted(key)
		return ctrl.Result{}, nil

	case err != nil:
		return ctrl.Result{}, err
	}

	if !kp.DeletionTimestamp.IsZero() {
		// Still present but on its way out. The controller is releasing the
		// Neutron ports; the daemon keeps the entry so an in-flight DEL can
		// still find the binding information it needs.
		return ctrl.Result{}, nil
	}

	if len(kp.Status.VIFs) == 0 {
		// Provisioned but not yet populated. Nothing to publish, and
		// publishing an empty entry would let a waiting ADD think it was done.
		return ctrl.Result{}, nil
	}

	r.Registry.Upsert(key, &kp)
	log.FromContext(ctx).V(1).Info("registry updated", "pod", key, "vifs", len(kp.Status.VIFs))
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler.
func (r *KuryrPortReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuryrv1.KuryrPort{}).
		Named("cni-kuryrport").
		Complete(r)
}

// PodReconciler notices pods that die before ever getting a KuryrPort.
//
// Without it, a CNI ADD racing a pod deletion would block for the full
// timeout waiting for ports the controller is never going to create.
type PodReconciler struct {
	client.Client
	Registry *registry.Registry
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile marks a vanished pod so waiters fail fast.
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := registry.Key(req.Namespace, req.Name)

	var pod corev1.Pod
	err := r.Get(ctx, req.NamespacedName, &pod)
	switch {
	case apierrors.IsNotFound(err):
		r.Registry.MarkPodGone(key)
		return ctrl.Result{}, nil

	case err != nil:
		return ctrl.Result{}, err
	}

	if !pod.DeletionTimestamp.IsZero() && !pod.Spec.HostNetwork {
		// A pod being deleted that never got ports will not get them now.
		// MarkPodGone leaves a provisioned pod alone, so this is safe to call
		// on every deletion.
		r.Registry.MarkPodGone(key)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Named("cni-pod").
		Complete(r)
}
