package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
)

func testScheme(t *testing.T) *runtime.Scheme {
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

func newPod(mods ...func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-0",
			Namespace: "default",
			UID:       types.UID("pod-uid-1"),
		},
		Spec:   corev1.PodSpec{NodeName: "worker-1"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, HostIP: "10.0.0.5"},
	}
	for _, m := range mods {
		m(pod)
	}
	return pod
}

func newPodReconciler(t *testing.T, objs ...client.Object) (*PodReconciler, client.Client) {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&kuryrv1.KuryrPort{}).
		Build()
	return &PodReconciler{Client: c, Scheme: s, Config: config.Defaults()}, c
}

func reconcilePod(t *testing.T, r *PodReconciler, pod *corev1.Pod) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func TestPodReconcilerCreatesKuryrPort(t *testing.T) {
	pod := newPod()
	r, c := newPodReconciler(t, pod)

	reconcilePod(t, r, pod)

	var kp kuryrv1.KuryrPort
	key := types.NamespacedName{Namespace: "default", Name: "web-0"}
	if err := c.Get(context.Background(), key, &kp); err != nil {
		t.Fatalf("expected a KuryrPort to be created: %v", err)
	}

	if kp.Spec.PodUID != pod.UID {
		t.Errorf("podUid = %q, want %q", kp.Spec.PodUID, pod.UID)
	}
	if kp.Spec.PodNodeName != "worker-1" {
		t.Errorf("podNodeName = %q, want worker-1", kp.Spec.PodNodeName)
	}
	// Without this label every CNI daemon in the cluster would stream every
	// KuryrPort, which is the whole point of setting it here.
	if got := kp.Labels[kuryrv1.NodeNameLabel]; got != "worker-1" {
		t.Errorf("node label = %q, want worker-1", got)
	}
	if !controllerutil.ContainsFinalizer(&kp, kuryrv1.KuryrPortFinalizer) {
		t.Error("KuryrPort is missing its finalizer")
	}
	if len(kp.OwnerReferences) != 1 || kp.OwnerReferences[0].UID != pod.UID {
		t.Errorf("owner references = %+v, want one pointing at the pod", kp.OwnerReferences)
	}

	var got corev1.Pod
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("reading pod back: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, kuryrv1.PodFinalizer) {
		t.Error("pod is missing the Kuryr finalizer")
	}
}

func TestPodReconcilerIgnoresHostNetwork(t *testing.T) {
	pod := newPod(func(p *corev1.Pod) { p.Spec.HostNetwork = true })
	r, c := newPodReconciler(t, pod)

	reconcilePod(t, r, pod)

	var kp kuryrv1.KuryrPort
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-0"}, &kp)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a host-network pod must not get a KuryrPort (err = %v)", err)
	}

	var got corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-0"}, &got); err != nil {
		t.Fatalf("reading pod back: %v", err)
	}
	if controllerutil.ContainsFinalizer(&got, kuryrv1.PodFinalizer) {
		t.Error("a host-network pod must not be given a finalizer we would then have to remove")
	}
}

func TestPodReconcilerWaitsForScheduling(t *testing.T) {
	pod := newPod(func(p *corev1.Pod) { p.Spec.NodeName = "" })
	r, c := newPodReconciler(t, pod)

	reconcilePod(t, r, pod)

	var kp kuryrv1.KuryrPort
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-0"}, &kp)
	if !apierrors.IsNotFound(err) {
		t.Fatal("an unscheduled pod has no node to bind a port to, so no KuryrPort should exist yet")
	}
}

func TestPodReconcilerIsIdempotent(t *testing.T) {
	pod := newPod()
	r, c := newPodReconciler(t, pod)

	for i := 0; i < 3; i++ {
		reconcilePod(t, r, pod)
		// Refresh so the next pass sees the finalizer written by the last.
		if err := c.Get(context.Background(),
			types.NamespacedName{Namespace: "default", Name: "web-0"}, pod); err != nil {
			t.Fatalf("reading pod back: %v", err)
		}
	}

	var list kuryrv1.KuryrPortList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("listing KuryrPorts: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d KuryrPorts after three reconciles, want exactly 1", len(list.Items))
	}
}

func TestPodReconcilerReplacesStaleKuryrPort(t *testing.T) {
	// A StatefulSet pod keeps its name but gets a new UID. The KuryrPort left
	// behind by the previous incarnation points at ports that belong to a pod
	// that no longer exists, and must not be reused.
	pod := newPod(func(p *corev1.Pod) { p.UID = types.UID("pod-uid-2") })
	stale := &kuryrv1.KuryrPort{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-0",
			Namespace: "default",
			Labels:    map[string]string{kuryrv1.NodeNameLabel: "worker-1"},
		},
		Spec: kuryrv1.KuryrPortSpec{PodUID: types.UID("pod-uid-1"), PodNodeName: "worker-1"},
	}
	r, c := newPodReconciler(t, pod, stale)

	res := reconcilePod(t, r, pod)
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue after deleting the stale KuryrPort")
	}

	var kp kuryrv1.KuryrPort
	key := types.NamespacedName{Namespace: "default", Name: "web-0"}
	err := c.Get(context.Background(), key, &kp)
	if err == nil && kp.Spec.PodUID == types.UID("pod-uid-1") {
		t.Fatal("the stale KuryrPort was left in place")
	}
}

func TestPodReconcilerReleasesCompletedPod(t *testing.T) {
	// A Job pod that has finished keeps its object but will never run again,
	// so its ports should be handed back rather than held until the object is
	// garbage collected.
	pod := newPod(func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodSucceeded
		p.Finalizers = []string{kuryrv1.PodFinalizer}
	})
	kp := &kuryrv1.KuryrPort{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "web-0",
			Namespace:  "default",
			Finalizers: []string{kuryrv1.KuryrPortFinalizer},
		},
		Spec: kuryrv1.KuryrPortSpec{PodUID: pod.UID, PodNodeName: "worker-1"},
	}
	r, c := newPodReconciler(t, pod, kp)

	reconcilePod(t, r, pod)

	var got kuryrv1.KuryrPort
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "web-0"}, &got); err != nil {
		t.Fatalf("reading KuryrPort back: %v", err)
	}
	if got.DeletionTimestamp.IsZero() {
		t.Error("expected the KuryrPort for a completed pod to be marked for deletion")
	}
}

func TestPodReconcilerFinalizeWithoutKuryrPort(t *testing.T) {
	// If there is no KuryrPort there is nothing in OpenStack to wait for, so
	// the pod finalizer must come off immediately rather than wedging the
	// delete.
	now := metav1.Now()
	pod := newPod(func(p *corev1.Pod) {
		p.DeletionTimestamp = &now
		p.Finalizers = []string{kuryrv1.PodFinalizer}
	})
	r, c := newPodReconciler(t, pod)

	reconcilePod(t, r, pod)

	var got corev1.Pod
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-0"}, &got)
	if err == nil && controllerutil.ContainsFinalizer(&got, kuryrv1.PodFinalizer) {
		t.Error("the pod finalizer should have been removed when no KuryrPort exists")
	}
}

func TestIsStaticPod(t *testing.T) {
	mirror := newPod(func(p *corev1.Pod) {
		p.Annotations = map[string]string{"kubernetes.io/config.mirror": "abc"}
	})
	if !isStaticPod(mirror) {
		t.Error("a mirror pod should be detected as static")
	}

	owned := newPod(func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "Node", Name: "worker-1"}}
	})
	if !isStaticPod(owned) {
		t.Error("a pod owned by a Node should be detected as static")
	}

	if isStaticPod(newPod()) {
		t.Error("an ordinary pod should not be detected as static")
	}
}
