package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
)

// releaseRecorder is a pod VIF driver that only records releases.
type releaseRecorder struct {
	drivers.PodVIFDriver
	released []string
}

func (f *releaseRecorder) ReleaseVIF(_ context.Context, v *kuryrv1.VIF, _ drivers.PodVIFRequest) error {
	f.released = append(f.released, v.ID)
	return nil
}

func deletedKuryrPort(pod *corev1.Pod) *kuryrv1.KuryrPort {
	now := metav1.Now()
	return &kuryrv1.KuryrPort{
		ObjectMeta: metav1.ObjectMeta{
			Name:              pod.Name,
			Namespace:         pod.Namespace,
			Finalizers:        []string{kuryrv1.KuryrPortFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: kuryrv1.KuryrPortSpec{PodUID: pod.UID, PodNodeName: pod.Spec.NodeName},
		Status: kuryrv1.KuryrPortStatus{VIFs: map[string]kuryrv1.PortVIF{
			"eth0": {Default: true, VIF: kuryrv1.VIF{ID: "port-1"}},
		}},
	}
}

func finalizeKuryrPort(t *testing.T, pod *corev1.Pod) (*releaseRecorder, client.Client, ctrl.Result) {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pod, deletedKuryrPort(pod)).
		WithStatusSubresource(&kuryrv1.KuryrPort{}).Build()
	vif := &releaseRecorder{}
	r := &KuryrPortReconciler{Client: c, Scheme: s, Config: config.Defaults(), VIF: vif, Project: fixedProject{id: "p"}}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return vif, c, res
}

// A KuryrPort deleted by hand must not take a running pod's port with it.
func TestKuryrPortDeletedUnderARunningPodKeepsItsPorts(t *testing.T) {
	pod := newPod(func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Finalizers = []string{kuryrv1.PodFinalizer}
	})
	vif, c, res := finalizeKuryrPort(t, pod)

	if len(vif.released) != 0 {
		t.Errorf("released %v while the pod still runs", vif.released)
	}
	if res.RequeueAfter == 0 {
		t.Error("no recheck scheduled, so the ports would never be released once the pod goes")
	}
	var kp kuryrv1.KuryrPort
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &kp); err != nil {
		t.Fatalf("the KuryrPort is gone: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&kp, kuryrv1.KuryrPortFinalizer) {
		t.Error("the KuryrPort finalizer was removed")
	}
}

func TestKuryrPortOfACompletedPodReleasesItsPorts(t *testing.T) {
	pod := newPod(func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodSucceeded
		p.Finalizers = []string{kuryrv1.PodFinalizer}
	})
	vif, _, _ := finalizeKuryrPort(t, pod)

	if len(vif.released) != 1 || vif.released[0] != "port-1" {
		t.Errorf("released = %v, want [port-1]", vif.released)
	}
}
