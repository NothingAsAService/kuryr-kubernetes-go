package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
)

// How long to wait before re-checking a port that Neutron has not activated.
// Ports normally go ACTIVE in a second or two; backing off further than this
// would add visible latency to every pod start.
const portActivationRequeue = 2 * time.Second

// portActivationTimeout is how long a port may sit DOWN before the controller
// says so on the KuryrPort. It does not stop the retries; it makes a stuck
// port visible to kubectl instead of only to whoever is reading logs.
const portActivationTimeout = 90 * time.Second

// KuryrPortReconciler is the OpenStack-facing half of pod networking: it owns
// every Neutron call in the pod lifecycle.
type KuryrPortReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *config.Config

	Project ProjectResolver
	Subnets SubnetResolver
	SGs     SecurityGroupResolver
	VIF     drivers.PodVIFDriver
	// MultiVIF adds the extra interfaces a pod asks for. Empty means every
	// pod gets exactly one.
	MultiVIF []drivers.MultiVIFDriver
}

// The resolver interfaces are narrowed copies of the driver interfaces so
// this package does not depend on driver construction, only on driver
// behaviour. It also makes the reconciler trivial to fake in tests.
type (
	// ProjectResolver picks the OpenStack project for an object.
	ProjectResolver = drivers.ProjectDriver
	// SubnetResolver picks the subnets for an object.
	SubnetResolver = drivers.SubnetDriver
	// SecurityGroupResolver picks the security groups for an object.
	SecurityGroupResolver = drivers.SecurityGroupsDriver
)

// +kubebuilder:rbac:groups=openstack.org,resources=kuryrports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrports/finalizers,verbs=update
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrnetworks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one KuryrPort from empty to fully active, or tears it down.
func (r *KuryrPortReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var kp kuryrv1.KuryrPort
	if err := r.Get(ctx, req.NamespacedName, &kp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !kp.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &kp)
	}

	if !controllerutil.ContainsFinalizer(&kp, kuryrv1.KuryrPortFinalizer) {
		patch := client.MergeFrom(kp.DeepCopy())
		controllerutil.AddFinalizer(&kp, kuryrv1.KuryrPortFinalizer)
		if err := r.Patch(ctx, &kp, patch); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	if len(kp.Status.VIFs) == 0 {
		return r.provision(ctx, &kp)
	}

	result, err := r.activate(ctx, &kp)
	if err != nil || !result.IsZero() {
		// Still coming up. Security groups are settled once, on a port that
		// has finished activating, rather than on every pass.
		return result, err
	}
	return r.syncSecurityGroups(ctx, &kp)
}

// syncSecurityGroups re-applies security groups to a running pod's ports when
// the set they should have has changed.
//
// This is how a NetworkPolicy reaches a pod that was already running when the
// policy was written, and how a pod that stops being selected gets its
// isolation removed. Nothing happens unless the groups actually differ, so
// the common case costs no Neutron call at all.
func (r *KuryrPortReconciler) syncSecurityGroups(ctx context.Context, kp *kuryrv1.KuryrPort) (ctrl.Result, error) {
	if !r.policyDriven() {
		// A fixed group list never changes, so there is nothing to follow.
		return ctrl.Result{}, nil
	}

	pod, err := r.podFor(ctx, kp)
	if pod == nil {
		return ctrl.Result{}, err
	}

	projectID, err := r.Project.ProjectFor(ctx, pod)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving project: %w", err)
	}

	want, err := r.SGs.SecurityGroupsFor(ctx, pod, projectID)
	if err != nil {
		if errors.Is(err, drivers.ErrRetryLater) {
			// A policy's security group has not been created yet. Wait for it
			// rather than leaving the pod on the wrong groups.
			log.FromContext(ctx).V(1).Info("security groups not ready yet", "reason", err)
			return ctrl.Result{RequeueAfter: portActivationRequeue}, nil
		}
		return ctrl.Result{}, fmt.Errorf("resolving security groups: %w", err)
	}

	if equalStrings(kp.Status.SecurityGroups, want) {
		return ctrl.Result{}, nil
	}

	for ifname, pv := range kp.Status.VIFs {
		v := pv.VIF
		if err := r.VIF.UpdateVIFSecurityGroups(ctx, &v, want); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying security groups to port %s (interface %s): %w",
				v.ID, ifname, err)
		}
	}

	patch := client.MergeFrom(kp.DeepCopy())
	kp.Status.SecurityGroups = want
	if err := r.Status().Patch(ctx, kp, patch); err != nil {
		// Neutron already has the new groups. Failing to record them only
		// means the next pass re-applies the same set, which is harmless.
		return ctrl.Result{}, fmt.Errorf("recording security groups on KuryrPort %s/%s: %w",
			kp.Namespace, kp.Name, err)
	}

	log.FromContext(ctx).Info("security groups updated", "securityGroups", want)
	return ctrl.Result{}, nil
}

// policyDriven reports whether security groups follow NetworkPolicies.
func (r *KuryrPortReconciler) policyDriven() bool {
	return r.Config.Kubernetes.PodSecurityGroupsDriver == "policy"
}

// equalStrings compares two lists that both arrive in a stable order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// provision creates the Neutron ports for a KuryrPort and records them.
func (r *KuryrPortReconciler) provision(ctx context.Context, kp *kuryrv1.KuryrPort) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pod, err := r.podFor(ctx, kp)
	if pod == nil {
		return ctrl.Result{}, err
	}

	projectID, err := r.Project.ProjectFor(ctx, pod)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving project: %w", err)
	}

	subnets, err := r.Subnets.SubnetsFor(ctx, pod, projectID)
	if err != nil {
		// A namespace whose network is still being built is an expected
		// intermediate state, not a failure.
		if errors.Is(err, drivers.ErrRetryLater) {
			r.setCondition(ctx, kp, kuryrv1.KuryrPortConditionProvisioned, metav1.ConditionFalse,
				"SubnetNotReady", err.Error())
			return ctrl.Result{RequeueAfter: portActivationRequeue}, nil
		}
		return ctrl.Result{}, fmt.Errorf("resolving subnets: %w", err)
	}

	sgs, err := r.SGs.SecurityGroupsFor(ctx, pod, projectID)
	if err != nil {
		// A NetworkPolicy selecting this pod whose security group is still
		// being created. Wait: creating the port on the wrong groups would
		// leave it unrestricted until the next pass noticed.
		if errors.Is(err, drivers.ErrRetryLater) {
			r.setCondition(ctx, kp, kuryrv1.KuryrPortConditionProvisioned, metav1.ConditionFalse,
				"SecurityGroupsNotReady", err.Error())
			return ctrl.Result{RequeueAfter: portActivationRequeue}, nil
		}
		return ctrl.Result{}, fmt.Errorf("resolving security groups: %w", err)
	}

	vifReq := drivers.PodVIFRequest{
		Pod:            pod,
		ProjectID:      projectID,
		Subnets:        subnets,
		SecurityGroups: sgs,
	}

	v, err := r.VIF.RequestVIF(ctx, vifReq)
	if err != nil {
		// A pool still recovering, or momentarily empty, is expected during a
		// controller restart or a burst of pods. Requeue quietly rather than
		// logging a stack of errors for something that resolves itself.
		if errors.Is(err, drivers.ErrRetryLater) {
			r.setCondition(ctx, kp, kuryrv1.KuryrPortConditionProvisioned, metav1.ConditionFalse,
				"WaitingForPort", err.Error())
			return ctrl.Result{RequeueAfter: portActivationRequeue}, nil
		}
		r.setCondition(ctx, kp, kuryrv1.KuryrPortConditionProvisioned, metav1.ConditionFalse,
			"RequestVIFFailed", err.Error())
		return ctrl.Result{}, fmt.Errorf("requesting VIF: %w", err)
	}

	// Extra interfaces from the NPWG annotation, if any. They are requested
	// before the status is written so that a single write records every port,
	// and a failure to write releases all of them together.
	vifs := map[string]kuryrv1.PortVIF{
		kuryrv1.DefaultIfName: {Default: true, VIF: *v},
	}
	extra, err := r.additionalVIFs(ctx, vifReq)
	if err != nil {
		logger.Error(err, "could not create the extra interfaces; releasing what was created")
		r.releaseAll(ctx, append(extra, *v), vifReq)
		r.setCondition(ctx, kp, kuryrv1.KuryrPortConditionProvisioned, metav1.ConditionFalse,
			"AdditionalVIFsFailed", err.Error())
		return ctrl.Result{}, err
	}
	for i := range extra {
		vifs[r.additionalIfName(i)] = kuryrv1.PortVIF{VIF: extra[i]}
	}

	// Record the ports before doing anything else. If this write fails they
	// are orphaned, so release them rather than leaking quota.
	patch := client.MergeFrom(kp.DeepCopy())
	kp.Status.VIFs = vifs
	// Record what the port was created with, so syncSecurityGroups only acts
	// on a real change rather than re-applying the same set once.
	kp.Status.SecurityGroups = sgs
	meta.SetStatusCondition(&kp.Status.Conditions, metav1.Condition{
		Type:    kuryrv1.KuryrPortConditionProvisioned,
		Status:  metav1.ConditionTrue,
		Reason:  "PortCreated",
		Message: v.String(),
	})
	if err := r.Status().Patch(ctx, kp, patch); err != nil {
		logger.Error(err, "could not record the ports; releasing them to avoid leaking quota")
		r.releaseAll(ctx, append(extra, *v), vifReq)
		return ctrl.Result{}, fmt.Errorf("recording VIFs on KuryrPort: %w", err)
	}

	logger.Info("provisioned pod ports", "vif", v.String(), "additional", len(extra))
	return ctrl.Result{RequeueAfter: portActivationRequeue}, nil
}

// additionalVIFs asks every configured multi-VIF driver what else this pod
// needs. Their results are concatenated, in configuration order.
func (r *KuryrPortReconciler) additionalVIFs(ctx context.Context,
	req drivers.PodVIFRequest) ([]kuryrv1.VIF, error) {

	var out []kuryrv1.VIF
	for _, drv := range r.MultiVIF {
		vifs, err := drv.AdditionalVIFs(ctx, req)
		// Ports created before the failure come back too, so the caller can
		// release them instead of leaking them.
		out = append(out, vifs...)
		if err != nil {
			return out, fmt.Errorf("multi-VIF driver %s: %w", drv.Name(), err)
		}
	}
	return out, nil
}

// additionalIfName is the name the nth extra interface gets inside the pod.
func (r *KuryrPortReconciler) additionalIfName(n int) string {
	prefix := r.Config.Kubernetes.AdditionalIfnamePrefix
	if prefix == "" {
		prefix = "eth"
	}
	// eth0 is the default interface, so extras start at 1.
	return fmt.Sprintf("%s%d", prefix, n+1)
}

// releaseAll gives back every port in a list, best effort.
//
// It is only called when something has already failed, so a further failure
// is reported and the rest are still attempted: stopping at the first one
// would leak the others for certain.
func (r *KuryrPortReconciler) releaseAll(ctx context.Context, vifs []kuryrv1.VIF,
	req drivers.PodVIFRequest) {

	logger := log.FromContext(ctx)
	for i := range vifs {
		if err := r.VIF.ReleaseVIF(ctx, &vifs[i], req); err != nil {
			logger.Error(err, "releasing a port failed", "portID", vifs[i].ID)
		}
	}
}

// activate waits for Neutron to bring every port to ACTIVE.
//
// The CNI daemon holds the pod ADD open until this flips, because an
// interface plugged before the dataplane is programmed drops traffic silently.
func (r *KuryrPortReconciler) activate(ctx context.Context, kp *kuryrv1.KuryrPort) (ctrl.Result, error) {
	if kp.Status.AllActive() {
		if meta.IsStatusConditionTrue(kp.Status.Conditions, kuryrv1.KuryrPortConditionActive) {
			return ctrl.Result{}, nil
		}
		r.setCondition(ctx, kp, kuryrv1.KuryrPortConditionActive, metav1.ConditionTrue,
			"PortsActive", "all Neutron ports are ACTIVE")
		return ctrl.Result{}, nil
	}

	pod, err := r.podFor(ctx, kp)
	if pod == nil {
		return ctrl.Result{}, err
	}
	projectID, err := r.Project.ProjectFor(ctx, pod)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving project: %w", err)
	}
	vifReq := drivers.PodVIFRequest{Pod: pod, ProjectID: projectID}

	changed := false
	updated := map[string]kuryrv1.PortVIF{}
	var pending *drivers.ErrPortNotReady

	for ifname, pv := range kp.Status.VIFs {
		v := pv.VIF
		if !v.Active {
			err := r.VIF.ActivateVIF(ctx, &v, vifReq)
			switch {
			case err == nil:
				changed = true
			case errors.As(err, &pending):
				// Still coming up; keep the old value and retry.
			default:
				return ctrl.Result{}, fmt.Errorf("activating port %s: %w", v.ID, err)
			}
		}
		updated[ifname] = kuryrv1.PortVIF{Default: pv.Default, VIF: v}
	}

	if changed {
		patch := client.MergeFrom(kp.DeepCopy())
		kp.Status.VIFs = updated
		if err := r.Status().Patch(ctx, kp, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording port activation: %w", err)
		}
		return ctrl.Result{RequeueAfter: portActivationRequeue}, nil
	}

	// Nothing became active this pass. Surface a slow port on the object once
	// it has been waiting long enough to be worth an operator's attention.
	if pending != nil && r.waitedLongerThan(kp, portActivationTimeout) {
		r.setCondition(ctx, kp, kuryrv1.KuryrPortConditionActive, metav1.ConditionFalse,
			"PortStuckInactive", pending.Error())
	}
	return ctrl.Result{RequeueAfter: portActivationRequeue}, nil
}

// finalize releases every OpenStack resource behind a KuryrPort, then frees
// both the pod and the KuryrPort itself.
func (r *KuryrPortReconciler) finalize(ctx context.Context, kp *kuryrv1.KuryrPort) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(kp, kuryrv1.KuryrPortFinalizer) {
		return ctrl.Result{}, nil
	}

	// The pod may already be gone. Release does not strictly need it, but the
	// nested driver uses its host IP to find the trunk, so pass it if we can.
	var pod *corev1.Pod
	var fetched corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: kp.Namespace, Name: kp.Name}, &fetched); err == nil {
		if fetched.UID == kp.Spec.PodUID {
			pod = &fetched
		}
	}

	projectID := ""
	if pod != nil {
		if id, err := r.Project.ProjectFor(ctx, pod); err == nil {
			projectID = id
		}
	}

	for ifname, pv := range kp.Status.VIFs {
		v := pv.VIF
		req := drivers.PodVIFRequest{Pod: pod, ProjectID: projectID}
		// A nested VIF records its trunk, so release works even with no pod.
		if err := r.VIF.ReleaseVIF(ctx, &v, req); err != nil {
			if pod == nil {
				// Best effort: the pod is gone, so there is nothing to hold
				// the finalizer open for, and refusing to release would leave
				// the object stuck forever.
				logger.Error(err, "could not release an orphaned port, continuing",
					"ifname", ifname, "portID", v.ID)
				continue
			}
			return ctrl.Result{}, fmt.Errorf("releasing port %s: %w", v.ID, err)
		}
		logger.Info("released pod port", "ifname", ifname, "portID", v.ID)
	}

	// Free the pod first: it is what a user is watching, and its finalizer is
	// the one that makes a delete appear to hang.
	if pod != nil && controllerutil.ContainsFinalizer(pod, kuryrv1.PodFinalizer) {
		patch := client.MergeFrom(pod.DeepCopy())
		controllerutil.RemoveFinalizer(pod, kuryrv1.PodFinalizer)
		if err := r.Patch(ctx, pod, patch); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("removing pod finalizer: %w", err)
		}
	}

	patch := client.MergeFrom(kp.DeepCopy())
	controllerutil.RemoveFinalizer(kp, kuryrv1.KuryrPortFinalizer)
	if err := r.Patch(ctx, kp, patch); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("removing KuryrPort finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// podFor fetches the pod a KuryrPort belongs to, dealing with the two ways it
// can be absent: deleted outright, or replaced by a pod of the same name.
func (r *KuryrPortReconciler) podFor(ctx context.Context, kp *kuryrv1.KuryrPort) (*corev1.Pod, error) {
	var pod corev1.Pod
	key := types.NamespacedName{Namespace: kp.Namespace, Name: kp.Name}

	err := r.Get(ctx, key, &pod)
	switch {
	case apierrors.IsNotFound(err):
		// The pod was force-deleted. Nothing will ever consume this KuryrPort,
		// so delete it and let finalize release whatever was created.
		log.FromContext(ctx).Info("pod is gone; deleting its KuryrPort so the ports get released")
		if err := r.Delete(ctx, kp); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("deleting orphaned KuryrPort: %w", err)
		}
		return nil, nil

	case err != nil:
		return nil, fmt.Errorf("reading pod for KuryrPort: %w", err)

	case pod.UID != kp.Spec.PodUID:
		log.FromContext(ctx).Info("KuryrPort belongs to an earlier pod of the same name; deleting it",
			"staleUID", kp.Spec.PodUID, "currentUID", pod.UID)
		if err := r.Delete(ctx, kp); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("deleting stale KuryrPort: %w", err)
		}
		return nil, nil
	}

	return &pod, nil
}

// waitedLongerThan reports whether the KuryrPort has existed for longer than d.
func (r *KuryrPortReconciler) waitedLongerThan(kp *kuryrv1.KuryrPort, d time.Duration) bool {
	return time.Since(kp.CreationTimestamp.Time) > d
}

// setCondition records a condition, ignoring a conflict: another pass will
// set it again, and failing a reconcile over a status message is not worth it.
func (r *KuryrPortReconciler) setCondition(ctx context.Context, kp *kuryrv1.KuryrPort,
	condType string, status metav1.ConditionStatus, reason, message string) {

	patch := client.MergeFrom(kp.DeepCopy())
	if !meta.SetStatusCondition(&kp.Status.Conditions, metav1.Condition{
		Type:    condType,
		Status:  status,
		Reason:  reason,
		Message: truncateMessage(message),
	}) {
		return
	}
	if err := r.Status().Patch(ctx, kp, patch); err != nil {
		log.FromContext(ctx).V(1).Info("could not update condition", "type", condType, "error", err)
	}
}

// truncateMessage keeps a condition message inside the API server limit.
func truncateMessage(s string) string {
	const limit = 32 * 1024
	if len(s) <= limit {
		return s
	}
	return s[:limit-3] + "..."
}

// SetupWithManager registers the reconciler.
//
// When security groups follow NetworkPolicies, two more things can change
// which groups a running pod should have: its own labels, and the policies in
// its namespace. Both are watched and mapped back onto the KuryrPort, which
// is what lets a policy reach pods that were already running.
func (r *KuryrPortReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&kuryrv1.KuryrPort{})

	if r.policyDriven() {
		b = b.Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(portForPod),
			builder.WithPredicates(predicate.LabelChangedPredicate{})).
			Watches(&kuryrv1.KuryrNetworkPolicy{},
				handler.EnqueueRequestsFromMapFunc(r.portsForPolicy))
	}

	return b.WithOptions(controller.Options{
		MaxConcurrentReconciles: r.Config.Kubernetes.MaxConcurrentReconciles,
	}).
		Named("kuryrport").
		Complete(r)
}

// portForPod maps a pod onto its KuryrPort, which carries the same name.
func portForPod(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.HostNetwork {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
	}}
}

// portsForPolicy maps a policy onto every KuryrPort in its namespace.
//
// Which pods a policy selects is decided by the security groups driver, and
// a policy that stops selecting a pod matters just as much as one that starts
// selecting it. Enqueuing the whole namespace covers both without repeating
// the selector logic here; a port whose groups are unchanged does no work.
func (r *KuryrPortReconciler) portsForPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	var list kuryrv1.KuryrPortList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing KuryrPorts after a policy change",
			"namespace", obj.GetNamespace())
		return nil
	}

	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			},
		})
	}
	return out
}
