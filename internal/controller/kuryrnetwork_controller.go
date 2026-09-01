package controller

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

// Pools is the part of the port pool this reconciler drives. It is nil when
// pooling is off, so every call site has to tolerate that.
type Pools interface {
	// DeleteNetworkPools empties the pools on a network before it is deleted.
	DeleteNetworkPools(ctx context.Context, networkID string) error
	// Populate primes the pool for one node and network.
	Populate(ctx context.Context, node *corev1.Node, req drivers.PodVIFRequest) error
}

// KuryrNetworkReconciler builds the Neutron network behind a namespace.
//
// Each step writes its result to the status before the next one starts. A
// controller that dies halfway resumes where it stopped instead of creating a
// second network it has no record of.
type KuryrNetworkReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Config    *config.Config
	OpenStack *openstack.Client

	// Pool is optional; it is nil when [kubernetes]vif_pool_driver is noop.
	Pool Pools
}

// +kubebuilder:rbac:groups=openstack.org,resources=kuryrnetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrnetworks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrnetworks/finalizers,verbs=update

// Reconcile drives one KuryrNetwork to having a network, a subnet and a
// router interface, or tears all three down.
func (r *KuryrNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var kn kuryrv1.KuryrNetwork
	if err := r.Get(ctx, req.NamespacedName, &kn); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading KuryrNetwork %s: %w", req.NamespacedName, err)
	}

	if !kn.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &kn)
	}

	if !controllerutil.ContainsFinalizer(&kn, kuryrv1.KuryrNetworkFinalizer) {
		patch := client.MergeFrom(kn.DeepCopy())
		controllerutil.AddFinalizer(&kn, kuryrv1.KuryrNetworkFinalizer)
		if err := r.Patch(ctx, &kn, patch); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("adding finalizer to KuryrNetwork %s: %w", kn.Name, err)
		}
	}

	if err := r.ensureNetwork(ctx, &kn); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureSubnet(ctx, &kn); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureRouter(ctx, &kn); err != nil {
		return ctrl.Result{}, err
	}
	r.primePools(ctx, &kn)
	if err := r.syncLabels(ctx, &kn); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.markReady(ctx, &kn)
}

// ensureNetwork creates the Neutron network if the status does not name one.
func (r *KuryrNetworkReconciler) ensureNetwork(ctx context.Context, kn *kuryrv1.KuryrNetwork) error {
	if kn.Status.NetID != "" {
		return nil
	}

	name := namespaceNetworkName(kn.Spec.NamespaceName, string(kn.UID))
	net, err := r.OpenStack.CreateNetwork(ctx, openstack.CreateNetworkSpec{
		Name:      name,
		ProjectID: kn.Spec.ProjectID,
		// The namespace UID goes in the description so a network can be
		// matched back to its namespace even if the status write below never
		// lands.
		Description: string(kn.UID),
	})
	if err != nil {
		return r.fail(ctx, kn, "CreateNetworkFailed", err)
	}

	kn.Status.NetID = net.ID
	if err := r.patchStatus(ctx, kn); err != nil {
		return err
	}
	log.FromContext(ctx).Info("created namespace network", "netID", net.ID, "name", name)
	return nil
}

// ensureSubnet allocates the namespace CIDR from the configured subnet pool.
func (r *KuryrNetworkReconciler) ensureSubnet(ctx context.Context, kn *kuryrv1.KuryrNetwork) error {
	if kn.Status.SubnetID != "" {
		return nil
	}
	if r.Config.NamespaceSubnet.PodSubnetPool == "" {
		return fmt.Errorf("[namespace_subnet]pod_subnet_pool is required to allocate a namespace subnet")
	}

	name := namespaceSubnetName(kn.Spec.NamespaceName, string(kn.UID))
	subnet, err := r.OpenStack.CreateSubnet(ctx, openstack.CreateSubnetSpec{
		Name:         name,
		NetworkID:    kn.Status.NetID,
		ProjectID:    kn.Spec.ProjectID,
		SubnetPoolID: r.Config.NamespaceSubnet.PodSubnetPool,
	})
	if err != nil {
		return r.fail(ctx, kn, "CreateSubnetFailed", err)
	}

	kn.Status.SubnetID = subnet.ID
	kn.Status.SubnetCIDR = subnet.CIDR
	if err := r.patchStatus(ctx, kn); err != nil {
		return err
	}
	log.FromContext(ctx).Info("created namespace subnet", "subnetID", subnet.ID, "cidr", subnet.CIDR)
	return nil
}

// ensureRouter attaches the subnet so the namespace can reach anything else.
func (r *KuryrNetworkReconciler) ensureRouter(ctx context.Context, kn *kuryrv1.KuryrNetwork) error {
	if kn.Status.RouterID != "" {
		return nil
	}

	routerID := r.Config.NamespaceSubnet.PodRouter
	if err := r.OpenStack.AttachSubnetToRouter(ctx, routerID, kn.Status.SubnetID); err != nil {
		return r.fail(ctx, kn, "AttachRouterFailed", err)
	}

	kn.Status.RouterID = routerID
	if err := r.patchStatus(ctx, kn); err != nil {
		return err
	}
	log.FromContext(ctx).Info("attached namespace subnet to router", "routerID", routerID)
	return nil
}

// syncLabels records the labels the security groups were last computed from.
func (r *KuryrNetworkReconciler) syncLabels(ctx context.Context, kn *kuryrv1.KuryrNetwork) error {
	if maps.Equal(kn.Status.NamespaceLabels, kn.Spec.NamespaceLabels) {
		return nil
	}
	kn.Status.NamespaceLabels = maps.Clone(kn.Spec.NamespaceLabels)
	return r.patchStatus(ctx, kn)
}

// markReady records that the namespace network is usable.
func (r *KuryrNetworkReconciler) markReady(ctx context.Context, kn *kuryrv1.KuryrNetwork) error {
	if meta.IsStatusConditionTrue(kn.Status.Conditions, kuryrv1.KuryrNetworkConditionReady) {
		return nil
	}
	meta.SetStatusCondition(&kn.Status.Conditions, metav1.Condition{
		Type:    kuryrv1.KuryrNetworkConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Provisioned",
		Message: fmt.Sprintf("network %s, subnet %s (%s)", kn.Status.NetID, kn.Status.SubnetID, kn.Status.SubnetCIDR),
	})
	return r.patchStatus(ctx, kn)
}

// fail records why a step could not complete and returns the error.
func (r *KuryrNetworkReconciler) fail(ctx context.Context, kn *kuryrv1.KuryrNetwork, reason string, cause error) error {
	meta.SetStatusCondition(&kn.Status.Conditions, metav1.Condition{
		Type:    kuryrv1.KuryrNetworkConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: truncateMessage(cause.Error()),
	})
	if err := r.patchStatus(ctx, kn); err != nil {
		// Report both: the original cause is what an operator needs, but a
		// failing status write is its own problem.
		return fmt.Errorf("%s: %w (recording the failure also failed: %w)", reason, cause, err)
	}
	return fmt.Errorf("%s: %w", reason, cause)
}

// finalize releases the Neutron resources, in the reverse order of creation.
func (r *KuryrNetworkReconciler) finalize(ctx context.Context, kn *kuryrv1.KuryrNetwork) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(kn, kuryrv1.KuryrNetworkFinalizer) {
		return ctrl.Result{}, nil
	}

	netID := kn.Status.NetID
	if netID == "" {
		// The status write may never have landed. Look the network up by the
		// namespace UID recorded in its description, so a crash between
		// creating and recording does not leak it.
		found, err := r.findOrphanedNetwork(ctx, kn)
		if err != nil {
			return ctrl.Result{}, err
		}
		netID = found
	}

	if netID != "" {
		// Ports must go before the network. A pool holding spare ports on it
		// would make Neutron refuse the delete.
		if r.Pool != nil {
			if err := r.Pool.DeleteNetworkPools(ctx, netID); err != nil {
				return ctrl.Result{}, fmt.Errorf("emptying port pools on network %s: %w", netID, err)
			}
		}

		if err := r.OpenStack.DetachSubnetFromRouter(ctx, kn.Status.RouterID, kn.Status.SubnetID); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.deleteNetwork(ctx, netID); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("released namespace network", "netID", netID)
	}

	patch := client.MergeFrom(kn.DeepCopy())
	controllerutil.RemoveFinalizer(kn, kuryrv1.KuryrNetworkFinalizer)
	if err := r.Patch(ctx, kn, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("removing finalizer from KuryrNetwork %s: %w", kn.Name, err)
	}
	return ctrl.Result{}, nil
}

// deleteNetwork removes the network, clearing leftover ports if Neutron
// complains that it is still in use.
func (r *KuryrNetworkReconciler) deleteNetwork(ctx context.Context, netID string) error {
	err := r.OpenStack.DeleteNetwork(ctx, netID)
	if err == nil {
		return nil
	}
	if !openstack.IsConflict(err) {
		return err
	}

	// Something still holds a port. Remove the ones that are ours and let the
	// next reconcile retry; anything else is not safe to delete.
	logger := log.FromContext(ctx)
	logger.Info("network still has ports, clearing the ones Kuryr owns", "netID", netID)

	var toDelete []string
	for _, owner := range []string{openstack.DeviceOwner, openstack.DeviceOwnerSubport, ""} {
		ports, listErr := r.OpenStack.ListPorts(ctx,
			openstack.PortFilter{NetworkID: netID, DeviceOwner: owner})
		if listErr != nil {
			return fmt.Errorf("listing leftover ports on network %s: %w", netID, listErr)
		}
		for _, port := range ports {
			// An empty device_owner is included because Neutron clears it
			// when a subport is detached, and those are ours too.
			if port.DeviceOwner == openstack.DeviceOwner ||
				port.DeviceOwner == openstack.DeviceOwnerSubport ||
				port.DeviceOwner == "" {
				toDelete = append(toDelete, port.ID)
			}
		}
	}

	if len(toDelete) == 0 {
		return fmt.Errorf("network %s is in use by ports Kuryr does not own: %w", netID, err)
	}
	if delErr := r.OpenStack.DeletePorts(ctx, toDelete); delErr != nil {
		return fmt.Errorf("deleting leftover ports on network %s: %w", netID, delErr)
	}

	// Requeue by reporting the original conflict; the ports are gone now, so
	// the next attempt should succeed.
	return fmt.Errorf("cleared %d leftover ports on network %s, retrying delete: %w",
		len(toDelete), netID, err)
}

// findOrphanedNetwork looks for a network created for this namespace whose ID
// was never written to the status.
func (r *KuryrNetworkReconciler) findOrphanedNetwork(ctx context.Context, kn *kuryrv1.KuryrNetwork) (string, error) {
	name := namespaceNetworkName(kn.Spec.NamespaceName, string(kn.UID))
	networks, err := r.OpenStack.FindNetworksByName(ctx, name)
	if err != nil {
		return "", fmt.Errorf("looking for an orphaned network for namespace %s: %w",
			kn.Spec.NamespaceName, err)
	}

	for _, net := range networks {
		// The description holds the namespace UID, which is what makes this
		// unambiguous when a namespace has been deleted and recreated.
		if net.Description == string(kn.UID) {
			log.FromContext(ctx).Info("found a network that was created but never recorded",
				"netID", net.ID)
			return net.ID, nil
		}
	}
	return "", nil
}

func (r *KuryrNetworkReconciler) patchStatus(ctx context.Context, kn *kuryrv1.KuryrNetwork) error {
	if err := r.Status().Update(ctx, kn); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("updating KuryrNetwork %s status: %w", kn.Name, err)
	}
	return nil
}

// SetupWithManager registers the reconciler.
func (r *KuryrNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuryrv1.KuryrNetwork{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.Config.Kubernetes.MaxConcurrentReconciles,
		}).
		Named("kuryrnetwork").
		Complete(r)
}

// primePools fills the port pools for this namespace's subnet on every node,
// so the first pod scheduled into it is served from a pool rather than waiting
// on a bulk create.
//
// Populated is recorded before the work rather than after. Priming twice would
// create a second batch on every node, while a prime that failed part way is
// made good by the ordinary top-up the first time a pod asks -- so the cheaper
// mistake to make is to skip.
//
// Failures are logged, not returned: a pool is an optimisation, and a cloud
// too busy to pre-create ports must not also stop the namespace from getting
// its network.
func (r *KuryrNetworkReconciler) primePools(ctx context.Context, kn *kuryrv1.KuryrNetwork) {
	if r.Pool == nil || kn.Status.Populated || kn.Status.SubnetID == "" {
		return
	}
	logger := log.FromContext(ctx)

	subnet, err := r.OpenStack.GetSubnet(ctx, kn.Status.SubnetID)
	if err != nil {
		logger.Error(err, "could not read the namespace subnet to prime the pools",
			"subnetID", kn.Status.SubnetID)
		return
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		logger.Error(err, "could not list nodes to prime the pools")
		return
	}

	patch := client.MergeFrom(kn.DeepCopy())
	kn.Status.Populated = true
	if err := r.Status().Patch(ctx, kn, patch); err != nil {
		// Priming without recording it would prime again on the next pass, so
		// give up rather than risk creating a batch per reconcile.
		logger.Error(err, "could not record that the pools were primed; skipping")
		return
	}

	req := drivers.PodVIFRequest{
		ProjectID:      kn.Spec.ProjectID,
		Subnets:        vif.SubnetSet{subnet.ID: subnet},
		SecurityGroups: r.Config.NeutronDefaults.PodSecurityGroups,
	}

	primed := 0
	for i := range nodes.Items {
		node := &nodes.Items[i]
		// A cordoned node will not be given pods, so ports pooled against it
		// would sit unused until someone uncordons it.
		if node.Spec.Unschedulable {
			continue
		}
		if err := r.Pool.Populate(ctx, node, req); err != nil {
			logger.V(1).Info("could not prime the pool for a node",
				"node", node.Name, "subnetID", subnet.ID, "error", err)
			continue
		}
		primed++
	}
	logger.Info("primed the port pools for the namespace subnet",
		"subnetID", subnet.ID, "nodes", primed)
}
