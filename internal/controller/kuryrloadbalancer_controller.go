package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

// lbProvisionRequeue is how long to wait while Octavia is building. A load
// balancer takes tens of seconds with the Amphora provider and rejects every
// change until it settles, so polling faster only wastes API calls.
const lbProvisionRequeue = 5 * time.Second

// KuryrLoadBalancerReconciler builds the Octavia topology behind a Service.
//
// Everything is driven from the spec and diffed against what the status says
// exists. Octavia only accepts changes while the load balancer is ACTIVE, so
// each pass does as much as it can and requeues rather than blocking on a
// provisioning poll the way the Python driver did.
type KuryrLoadBalancerReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Config    *config.Config
	OpenStack loadBalancerCloud

	// PublicIP acquires the external address of a LoadBalancer Service. It
	// is nil when external_svc_net is unset, in which case such Services are
	// reported as unsupported rather than silently left without an address.
	PublicIP drivers.PublicIPDriver

	// PodSubnets resolves the subnet of a backend pod, needed only in L2
	// member mode.
	PodSubnets SubnetResolver
	// PodProject resolves the project of a backend pod, for the same reason.
	PodProject ProjectResolver
}

// loadBalancerCloud is the slice of OpenStack this reconciler uses. It is an
// interface so the listener, pool and member diffing can be tested without a
// cloud, which is where the real complexity lives.
type loadBalancerCloud interface {
	CreateLoadBalancer(ctx context.Context, spec openstack.LoadBalancerSpec) (*openstack.LoadBalancer, error)
	GetLoadBalancer(ctx context.Context, id string) (*openstack.LoadBalancer, error)
	FindLoadBalancersByName(ctx context.Context, name, projectID string) ([]*openstack.LoadBalancer, error)
	DeleteLoadBalancer(ctx context.Context, id string) error

	CreateListener(ctx context.Context, l openstack.Listener,
		timeoutClientData, timeoutMemberData int32) (*openstack.Listener, error)
	DeleteListener(ctx context.Context, id string) error

	CreatePool(ctx context.Context, p openstack.Pool) (*openstack.Pool, error)
	DeletePool(ctx context.Context, id string) error

	CreateMember(ctx context.Context, m openstack.Member) (*openstack.Member, error)
	DeleteMember(ctx context.Context, poolID, memberID string) error

	GetPort(ctx context.Context, id string) (*openstack.Port, error)
	GetSubnet(ctx context.Context, id string) (*openstack.Subnet, error)
	ListSGRules(ctx context.Context, sgID string) ([]*openstack.SGRule, error)
	CreateSGRule(ctx context.Context, r openstack.SGRule) (*openstack.SGRule, error)
	DeleteSGRule(ctx context.Context, id string) error
}

var _ loadBalancerCloud = (*openstack.Client)(nil)

// +kubebuilder:rbac:groups=openstack.org,resources=kuryrloadbalancers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrloadbalancers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrloadbalancers/finalizers,verbs=update

// Reconcile makes Octavia match the load balancer the spec describes.
func (r *KuryrLoadBalancerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var klb kuryrv1.KuryrLoadBalancer
	if err := r.Get(ctx, req.NamespacedName, &klb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading KuryrLoadBalancer %s: %w", req.NamespacedName, err)
	}

	if !klb.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &klb)
	}

	if !controllerutil.ContainsFinalizer(&klb, kuryrv1.KuryrLoadBalancerFinalizer) {
		patch := client.MergeFrom(klb.DeepCopy())
		controllerutil.AddFinalizer(&klb, kuryrv1.KuryrLoadBalancerFinalizer)
		if err := r.Patch(ctx, &klb, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer to KuryrLoadBalancer %s: %w",
				req.NamespacedName, err)
		}
	}

	if klb.Spec.IP == "" || klb.Spec.SubnetID == "" {
		// The Service controller has not filled the spec in yet.
		return ctrl.Result{}, nil
	}

	result, err := r.build(ctx, &klb)
	if err != nil {
		r.fail(ctx, &klb, "ProvisioningFailed", err)
		return ctrl.Result{}, err
	}
	return result, nil
}

// build runs the provisioning steps in order, stopping at the first one that
// has to wait.
func (r *KuryrLoadBalancerReconciler) build(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer) (ctrl.Result, error) {
	lb, result, err := r.ensureLoadBalancer(ctx, klb)
	if lb == nil {
		return result, err
	}

	if err := r.recordSecurityGroups(ctx, klb); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncListeners(ctx, klb); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncPools(ctx, klb); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncMembers(ctx, klb); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncVIPSecurityRules(ctx, klb, lb); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensurePublicIP(ctx, klb); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.updateServiceStatus(ctx, klb); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.setCondition(ctx, klb, metav1.ConditionTrue, "Provisioned",
		fmt.Sprintf("load balancer %s serves %d listeners and %d members",
			klb.Status.LoadBalancer.IP, len(klb.Status.Listeners), len(klb.Status.Members)))
}

// ensureLoadBalancer creates the load balancer and waits for it to settle.
//
// A nil result means the caller should stop and come back: either the load
// balancer is still provisioning or something failed.
func (r *KuryrLoadBalancerReconciler) ensureLoadBalancer(ctx context.Context,
	klb *kuryrv1.KuryrLoadBalancer) (*openstack.LoadBalancer, ctrl.Result, error) {

	logger := log.FromContext(ctx)

	if klb.Status.LoadBalancer != nil {
		lb, err := r.OpenStack.GetLoadBalancer(ctx, klb.Status.LoadBalancer.ID)
		if err != nil {
			if openstack.IsNotFound(err) {
				// Something outside Kuryr deleted it. Forget it and rebuild
				// from scratch on the next pass; the recorded listeners and
				// members went with it.
				logger.Info("recorded load balancer no longer exists, rebuilding",
					"loadBalancerID", klb.Status.LoadBalancer.ID)
				if err := r.forgetTopology(ctx, klb); err != nil {
					return nil, ctrl.Result{}, err
				}
				return nil, ctrl.Result{RequeueAfter: lbProvisionRequeue}, nil
			}
			return nil, ctrl.Result{}, err
		}

		// A provider change means a different Octavia driver, which cannot be
		// applied in place. The load balancer is released so the next pass
		// builds it again on the new provider.
		if wanted := klb.Spec.Provider; wanted != "" && lb.Provider != "" && lb.Provider != wanted {
			logger.Info("Octavia provider changed, releasing the load balancer",
				"from", lb.Provider, "to", wanted)
			if err := r.releaseTopology(ctx, klb); err != nil {
				return nil, ctrl.Result{}, err
			}
			return nil, ctrl.Result{RequeueAfter: lbProvisionRequeue}, nil
		}

		switch lb.ProvisioningStatus {
		case openstack.ProvisioningActive:
			return lb, ctrl.Result{}, nil
		case openstack.ProvisioningError:
			// Octavia will not recover this on its own. Say so on the object
			// and release it so the next pass starts clean.
			r.fail(ctx, klb, "LoadBalancerError",
				fmt.Errorf("load balancer %s is in ERROR in Octavia; releasing it to retry", lb.ID))
			if err := r.releaseTopology(ctx, klb); err != nil {
				return nil, ctrl.Result{}, err
			}
			return nil, ctrl.Result{RequeueAfter: lbProvisionRequeue}, nil
		default:
			logger.V(1).Info("load balancer is still provisioning",
				"loadBalancerID", lb.ID, "status", lb.ProvisioningStatus)
			return nil, ctrl.Result{RequeueAfter: lbProvisionRequeue}, nil
		}
	}

	name := loadBalancerName(klb.Namespace, klb.Name, string(klb.UID))

	// A previous pass may have created the load balancer and failed before
	// recording it. Adopting it keeps a retry from leaking one.
	existing, err := r.OpenStack.FindLoadBalancersByName(ctx, name, klb.Spec.ProjectID)
	if err != nil {
		return nil, ctrl.Result{}, err
	}

	var lb *openstack.LoadBalancer
	switch len(existing) {
	case 0:
		lb, err = r.OpenStack.CreateLoadBalancer(ctx, openstack.LoadBalancerSpec{
			Name:       name,
			ProjectID:  klb.Spec.ProjectID,
			SubnetID:   klb.Spec.SubnetID,
			VIPAddress: klb.Spec.IP,
			Provider:   klb.Spec.Provider,
		})
		if err != nil {
			return nil, ctrl.Result{}, err
		}
		logger.Info("load balancer created", "loadBalancerID", lb.ID, "vip", lb.VIPAddress)
	case 1:
		lb = existing[0]
		logger.Info("adopted an existing load balancer", "loadBalancerID", lb.ID)
	default:
		// The name carries the Service UID, so duplicates cannot belong to
		// two different Services.
		return nil, ctrl.Result{}, fmt.Errorf(
			"found %d load balancers named %q; expected at most one, remove the duplicates",
			len(existing), name)
	}

	patch := client.MergeFrom(klb.DeepCopy())
	klb.Status.LoadBalancer = &kuryrv1.LoadBalancer{
		ID:        lb.ID,
		Name:      lb.Name,
		ProjectID: lb.ProjectID,
		SubnetID:  klb.Spec.SubnetID,
		IP:        klb.Spec.IP,
		PortID:    lb.VIPPortID,
		Provider:  lb.Provider,
	}
	if err := r.Status().Patch(ctx, klb, patch); err != nil {
		// The load balancer exists but is unrecorded; the lookup above adopts
		// it next time.
		return nil, ctrl.Result{}, fmt.Errorf("recording load balancer %s on KuryrLoadBalancer %s/%s: %w",
			lb.ID, klb.Namespace, klb.Name, err)
	}

	// Freshly created, so it is certainly not ACTIVE yet.
	return nil, ctrl.Result{RequeueAfter: lbProvisionRequeue}, nil
}

// recordSecurityGroups notes which security groups the backends of this
// Service carry.
//
// They are deliberately *not* applied to the VIP port. Octavia creates and
// owns a security group on that port, and overwriting it would cut the load
// balancer off from its own control plane. What these groups are for is
// deriving the VIP rules in syncVIPSecurityRules: they say what the backends
// allow, which is what the VIP has to allow too once the amphora has
// rewritten the source address.
func (r *KuryrLoadBalancerReconciler) recordSecurityGroups(ctx context.Context,
	klb *kuryrv1.KuryrLoadBalancer) error {

	want := slices.Sorted(slices.Values(klb.Spec.SecurityGroupIDs))

	if equalStrings(klb.Status.LoadBalancer.SecurityGroups, want) {
		return nil
	}

	patch := client.MergeFrom(klb.DeepCopy())
	klb.Status.LoadBalancer.SecurityGroups = want
	if err := r.Status().Patch(ctx, klb, patch); err != nil {
		return fmt.Errorf("recording the backend security groups of KuryrLoadBalancer %s/%s: %w",
			klb.Namespace, klb.Name, err)
	}
	log.FromContext(ctx).V(1).Info("backend security groups recorded", "securityGroups", want)
	return nil
}

// syncListeners creates one listener per service port and removes the rest.
func (r *KuryrLoadBalancerReconciler) syncListeners(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer) error {
	lb := klb.Status.LoadBalancer
	lbName := lb.Name

	have := map[string]kuryrv1.Listener{}
	for _, l := range klb.Status.Listeners {
		have[listenerKey(string(l.Protocol), l.Port)] = l
	}

	kept := map[string]bool{}
	next := make([]kuryrv1.Listener, 0, len(klb.Spec.Ports))
	created := 0

	for _, port := range klb.Spec.Ports {
		key := listenerKey(string(port.Protocol), port.Port)
		if kept[key] {
			// Two service ports on the same protocol and port cannot both
			// have a listener; the first one wins.
			continue
		}
		kept[key] = true

		if existing, ok := have[key]; ok {
			next = append(next, existing)
			continue
		}

		l, err := r.OpenStack.CreateListener(ctx, openstack.Listener{
			Name:           listenerName(lbName, string(port.Protocol), port.Port),
			ProjectID:      lb.ProjectID,
			LoadBalancerID: lb.ID,
			Protocol:       string(port.Protocol),
			Port:           port.Port,
		}, klb.Spec.TimeoutClientData, klb.Spec.TimeoutMemberData)
		if err != nil {
			// Record what exists before giving up, so the retry picks up here.
			if saveErr := r.save(ctx, klb, "listeners", func() { klb.Status.Listeners = next }); saveErr != nil {
				return fmt.Errorf("%w (and recording the listeners created so far failed: %w)",
					err, saveErr)
			}
			return err
		}
		next = append(next, kuryrv1.Listener{
			ID:                l.ID,
			Name:              l.Name,
			ProjectID:         l.ProjectID,
			LoadBalancerID:    lb.ID,
			Protocol:          port.Protocol,
			Port:              port.Port,
			TimeoutClientData: klb.Spec.TimeoutClientData,
			TimeoutMemberData: klb.Spec.TimeoutMemberData,
		})
		created++
	}

	removed := 0
	for key, existing := range have {
		if kept[key] {
			continue
		}
		if err := r.OpenStack.DeleteListener(ctx, existing.ID); err != nil {
			if saveErr := r.save(ctx, klb, "listeners", func() { klb.Status.Listeners = next }); saveErr != nil {
				return fmt.Errorf("%w (and recording the surviving listeners failed: %w)",
					err, saveErr)
			}
			return err
		}
		removed++
	}

	if created == 0 && removed == 0 {
		return nil
	}
	if err := r.save(ctx, klb, "listeners", func() { klb.Status.Listeners = next }); err != nil {
		return err
	}
	log.FromContext(ctx).Info("listeners synced", "created", created, "removed", removed)
	return nil
}

// syncPools gives every listener exactly one pool.
func (r *KuryrLoadBalancerReconciler) syncPools(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer) error {
	lb := klb.Status.LoadBalancer

	have := map[string]kuryrv1.Pool{}
	for _, p := range klb.Status.Pools {
		have[p.ListenerID] = p
	}

	kept := map[string]bool{}
	next := make([]kuryrv1.Pool, 0, len(klb.Status.Listeners))
	created := 0

	for _, l := range klb.Status.Listeners {
		kept[l.ID] = true
		if existing, ok := have[l.ID]; ok {
			next = append(next, existing)
			continue
		}

		p, err := r.OpenStack.CreatePool(ctx, openstack.Pool{
			Name:           l.Name,
			ProjectID:      lb.ProjectID,
			LoadBalancerID: lb.ID,
			ListenerID:     l.ID,
			Protocol:       string(l.Protocol),
			Algorithm:      r.Config.OctaviaDefaults.LBAlgorithm,
		})
		if err != nil {
			if saveErr := r.save(ctx, klb, "pools", func() { klb.Status.Pools = next }); saveErr != nil {
				return fmt.Errorf("%w (and recording the pools created so far failed: %w)",
					err, saveErr)
			}
			return err
		}
		next = append(next, kuryrv1.Pool{
			ID:             p.ID,
			Name:           p.Name,
			ProjectID:      p.ProjectID,
			LoadBalancerID: lb.ID,
			ListenerID:     l.ID,
			Protocol:       l.Protocol,
		})
		created++
	}

	removed := 0
	for listenerID, existing := range have {
		if kept[listenerID] {
			continue
		}
		// The listener is gone, so its pool goes too. Deleting the pool also
		// deletes its members, which is why they are dropped from the status
		// below rather than deleted one by one.
		if err := r.OpenStack.DeletePool(ctx, existing.ID); err != nil {
			if saveErr := r.save(ctx, klb, "pools", func() { klb.Status.Pools = next }); saveErr != nil {
				return fmt.Errorf("%w (and recording the surviving pools failed: %w)",
					err, saveErr)
			}
			return err
		}
		removed++
	}

	if created == 0 && removed == 0 {
		return nil
	}
	if err := r.save(ctx, klb, "pools", func() { klb.Status.Pools = next }); err != nil {
		return err
	}
	log.FromContext(ctx).Info("pools synced", "created", created, "removed", removed)
	return nil
}

// syncMembers registers every ready backend in the pool for its port.
func (r *KuryrLoadBalancerReconciler) syncMembers(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer) error {
	lb := klb.Status.LoadBalancer

	// Pools no longer exist for listeners that went away; drop their members
	// from the status rather than trying to delete them individually.
	livePools := map[string]kuryrv1.Pool{}
	for _, p := range klb.Status.Pools {
		livePools[p.ID] = p
	}

	desired, err := r.desiredMembers(ctx, klb)
	if err != nil {
		return err
	}

	have := map[string]kuryrv1.Member{}
	for _, m := range klb.Status.Members {
		if _, ok := livePools[m.PoolID]; !ok {
			continue
		}
		have[memberKey(m.PoolID, m.IP, m.Port)] = m
	}

	kept := map[string]bool{}
	next := make([]kuryrv1.Member, 0, len(desired))
	created := 0

	for _, want := range desired {
		key := memberKey(want.PoolID, want.IP, want.Port)
		if kept[key] {
			continue
		}
		kept[key] = true

		if existing, ok := have[key]; ok {
			next = append(next, existing)
			continue
		}

		m, err := r.OpenStack.CreateMember(ctx, openstack.Member{
			Name:     memberName(lb.Name, want.IP, want.Port),
			PoolID:   want.PoolID,
			SubnetID: want.SubnetID,
			Address:  want.IP,
			Port:     want.Port,
		})
		if err != nil {
			if saveErr := r.save(ctx, klb, "members", func() { klb.Status.Members = next }); saveErr != nil {
				return fmt.Errorf("%w (and recording the members created so far failed: %w)",
					err, saveErr)
			}
			return err
		}
		want.ID = m.ID
		want.Name = m.Name
		want.ProjectID = lb.ProjectID
		next = append(next, want)
		created++
	}

	removed := 0
	for key, existing := range have {
		if kept[key] {
			continue
		}
		if err := r.OpenStack.DeleteMember(ctx, existing.PoolID, existing.ID); err != nil {
			if saveErr := r.save(ctx, klb, "members", func() { klb.Status.Members = next }); saveErr != nil {
				return fmt.Errorf("%w (and recording the surviving members failed: %w)",
					err, saveErr)
			}
			return err
		}
		removed++
	}

	if created == 0 && removed == 0 && len(next) == len(klb.Status.Members) {
		return nil
	}
	if err := r.save(ctx, klb, "members", func() { klb.Status.Members = next }); err != nil {
		return err
	}
	log.FromContext(ctx).Info("members synced",
		"created", created, "removed", removed, "total", len(next))
	return nil
}

// desiredMembers works out which backend belongs in which pool.
//
// An EndpointSlice port carries the *Service* port name and the resolved
// container port, so matching by name is what turns a named targetPort into
// a number. Doing it any other way would mean resolving the name against
// every backing pod.
func (r *KuryrLoadBalancerReconciler) desiredMembers(ctx context.Context,
	klb *kuryrv1.KuryrLoadBalancer) ([]kuryrv1.Member, error) {

	poolByListener := map[string]kuryrv1.Pool{}
	for _, p := range klb.Status.Pools {
		poolByListener[p.ListenerID] = p
	}

	var out []kuryrv1.Member
	for _, listener := range klb.Status.Listeners {
		pool, ok := poolByListener[listener.ID]
		if !ok {
			// The pool for this listener has not been created yet; the next
			// pass will pick it up.
			continue
		}

		servicePort, ok := portFor(klb.Spec.Ports, listener)
		if !ok {
			// The listener outlived its service port. syncListeners removes
			// it; nothing to add here.
			continue
		}

		for i := range klb.Spec.Subsets {
			subset := &klb.Spec.Subsets[i]
			memberPort, ok := backendPort(subset, servicePort)
			if !ok {
				continue
			}
			for _, addr := range subset.Addresses {
				if !addr.Ready {
					continue
				}
				subnetID, err := r.memberSubnet(ctx, klb, addr)
				if err != nil {
					return nil, err
				}
				out = append(out, kuryrv1.Member{
					PoolID:   pool.ID,
					SubnetID: subnetID,
					IP:       addr.IP,
					Port:     memberPort,
				})
			}
		}
	}

	slices.SortFunc(out, func(a, b kuryrv1.Member) int {
		return cmp.Or(
			cmp.Compare(a.PoolID, b.PoolID),
			cmp.Compare(a.IP, b.IP),
			cmp.Compare(a.Port, b.Port),
		)
	})
	return out, nil
}

// memberSubnet returns the subnet to register a backend on.
//
// In L3 mode, the default, the load balancer reaches members through a
// router, so Octavia is told the load balancer's own subnet. In L2 mode the
// traffic is bridged and Octavia needs the subnet the backend actually sits
// on.
func (r *KuryrLoadBalancerReconciler) memberSubnet(ctx context.Context,
	klb *kuryrv1.KuryrLoadBalancer, addr kuryrv1.EndpointAddress) (string, error) {

	if r.Config.OctaviaDefaults.MemberMode != "L2" {
		return klb.Status.LoadBalancer.SubnetID, nil
	}

	// A backend with no pod behind it is on a node, not a pod network.
	if addr.TargetRef == nil || addr.TargetRef.Kind != "Pod" {
		return r.nodeSubnetFor(ctx, addr.IP)
	}

	var pod corev1.Pod
	key := types.NamespacedName{Namespace: addr.TargetRef.Namespace, Name: addr.TargetRef.Name}
	if key.Namespace == "" {
		key.Namespace = klb.Namespace
	}
	if err := r.Get(ctx, key, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			// The pod went away between the EndpointSlice being written and
			// now. Treat it as a node address; the slice will catch up.
			return r.nodeSubnetFor(ctx, addr.IP)
		}
		return "", fmt.Errorf("reading backend pod %s: %w", key, err)
	}
	if pod.Spec.HostNetwork {
		return r.nodeSubnetFor(ctx, addr.IP)
	}

	projectID, err := r.PodProject.ProjectFor(ctx, &pod)
	if err != nil {
		return "", fmt.Errorf("resolving project of backend pod %s: %w", key, err)
	}
	subnets, err := r.PodSubnets.SubnetsFor(ctx, &pod, projectID)
	if err != nil {
		return "", fmt.Errorf("resolving subnets of backend pod %s: %w", key, err)
	}
	for id, subnet := range subnets {
		contains, err := cidrContains(subnet.CIDR, addr.IP)
		if err != nil {
			return "", err
		}
		if contains {
			return id, nil
		}
	}

	// The address is not on the pod subnet, which means the backend is on the
	// node network after all.
	return r.nodeSubnetFor(ctx, addr.IP)
}

// nodeSubnetFor finds which configured worker node subnet an address is on.
func (r *KuryrLoadBalancerReconciler) nodeSubnetFor(ctx context.Context, ip string) (string, error) {
	ids := r.Config.PodVIFNested.WorkerNodesSubnets
	if len(ids) == 0 {
		return "", fmt.Errorf("backend %s is not on a pod subnet and "+
			"[pod_vif_nested]worker_nodes_subnets is empty, so its subnet cannot be determined; "+
			"set it, or use the default L3 member mode", ip)
	}

	for _, id := range ids {
		subnet, err := r.OpenStack.GetSubnet(ctx, id)
		if err != nil {
			return "", fmt.Errorf("reading worker node subnet %s: %w", id, err)
		}
		contains, err := cidrContains(subnet.CIDR, ip)
		if err != nil {
			return "", err
		}
		if contains {
			return id, nil
		}
	}

	return "", fmt.Errorf("backend %s is on none of the configured worker node subnets %v", ip, ids)
}

// ensurePublicIP gives a LoadBalancer Service its external address.
func (r *KuryrLoadBalancerReconciler) ensurePublicIP(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer) error {
	if klb.Spec.Type != string(corev1.ServiceTypeLoadBalancer) {
		return nil
	}
	if klb.Status.PublicIP != nil {
		return nil
	}
	if r.PublicIP == nil {
		return fmt.Errorf("no external network is configured but Service %s/%s is of type "+
			"LoadBalancer; set [neutron_defaults]external_svc_net", klb.Namespace, klb.Name)
	}

	portID := klb.Status.LoadBalancer.PortID
	ip, err := r.PublicIP.Acquire(ctx, klb, klb.Spec.ProjectID, portID)
	if err != nil {
		return err
	}
	if ip == nil {
		return nil
	}

	if err := r.PublicIP.Associate(ctx, ip, portID); err != nil {
		// The address exists but points nowhere. Release it rather than leak
		// it, and report both problems if that fails too.
		if relErr := r.PublicIP.Release(ctx, ip); relErr != nil {
			return fmt.Errorf("%w (and releasing the unassociated address failed: %w)", err, relErr)
		}
		return err
	}

	// releaseTopology reads this back as AllocMethod == "pool", so the two
	// spellings have to stay in step.
	allocMethod := "user"
	if ip.FromPool {
		allocMethod = "pool"
	}

	patch := client.MergeFrom(klb.DeepCopy())
	klb.Status.PublicIP = &kuryrv1.PublicIPInfo{
		IPID:        ip.ID,
		IPAddr:      ip.Address,
		AllocMethod: allocMethod,
	}
	if err := r.Status().Patch(ctx, klb, patch); err != nil {
		// The address is associated but unrecorded, so nothing would ever
		// release it. Undo the association to keep the state consistent.
		if relErr := r.PublicIP.Release(ctx, ip); relErr != nil {
			return fmt.Errorf("recording the public IP failed (%v) and releasing it also "+
				"failed: %w", err, relErr)
		}
		return fmt.Errorf("recording the public IP of KuryrLoadBalancer %s/%s: %w",
			klb.Namespace, klb.Name, err)
	}

	log.FromContext(ctx).Info("public IP associated", "address", ip.Address)
	return nil
}

// updateServiceStatus publishes the external address on the Service, which is
// what makes `kubectl get svc` show something other than <pending>.
func (r *KuryrLoadBalancerReconciler) updateServiceStatus(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer) error {
	if klb.Spec.Type != string(corev1.ServiceTypeLoadBalancer) || klb.Status.PublicIP == nil {
		return nil
	}

	var svc corev1.Service
	key := types.NamespacedName{Namespace: klb.Namespace, Name: klb.Name}
	if err := r.Get(ctx, key, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			// The Service is gone; this object is about to be garbage
			// collected with it.
			return nil
		}
		return fmt.Errorf("reading Service %s to publish its address: %w", key, err)
	}

	want := klb.Status.PublicIP.IPAddr
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP == want {
			return nil
		}
	}

	patch := client.MergeFrom(svc.DeepCopy())
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: want}}
	if err := r.Status().Patch(ctx, &svc, patch); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("publishing the load balancer address on Service %s: %w", key, err)
	}
	return nil
}

// finalize releases everything the load balancer owns.
func (r *KuryrLoadBalancerReconciler) finalize(ctx context.Context,
	klb *kuryrv1.KuryrLoadBalancer) (ctrl.Result, error) {

	if !controllerutil.ContainsFinalizer(klb, kuryrv1.KuryrLoadBalancerFinalizer) {
		return ctrl.Result{}, nil
	}

	if err := r.releaseTopology(ctx, klb); err != nil {
		return ctrl.Result{}, err
	}

	patch := client.MergeFrom(klb.DeepCopy())
	controllerutil.RemoveFinalizer(klb, kuryrv1.KuryrLoadBalancerFinalizer)
	if err := r.Patch(ctx, klb, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("removing finalizer from KuryrLoadBalancer %s/%s: %w",
			klb.Namespace, klb.Name, err)
	}
	return ctrl.Result{}, nil
}

// releaseTopology deletes the load balancer and the public IP, then clears
// the status so a later pass rebuilds from nothing.
func (r *KuryrLoadBalancerReconciler) releaseTopology(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer) error {
	if info := klb.Status.PublicIP; info != nil && r.PublicIP != nil {
		ip := &drivers.PublicIP{
			ID:       info.IPID,
			Address:  info.IPAddr,
			FromPool: info.AllocMethod == "pool",
		}
		if err := r.PublicIP.Release(ctx, ip); err != nil {
			return fmt.Errorf("releasing the public IP of KuryrLoadBalancer %s/%s: %w",
				klb.Namespace, klb.Name, err)
		}
	}

	if lb := klb.Status.LoadBalancer; lb != nil {
		// The delete cascades, so listeners, pools and members go with it.
		if err := r.OpenStack.DeleteLoadBalancer(ctx, lb.ID); err != nil {
			return err
		}
		log.FromContext(ctx).Info("load balancer released", "loadBalancerID", lb.ID)
	}

	return r.forgetTopology(ctx, klb)
}

// forgetTopology clears the recorded Octavia resources.
func (r *KuryrLoadBalancerReconciler) forgetTopology(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer) error {
	patch := client.MergeFrom(klb.DeepCopy())
	klb.Status.LoadBalancer = nil
	klb.Status.Listeners = nil
	klb.Status.Pools = nil
	klb.Status.Members = nil
	klb.Status.PublicIP = nil
	if err := r.Status().Patch(ctx, klb, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("clearing the status of KuryrLoadBalancer %s/%s: %w",
			klb.Namespace, klb.Name, err)
	}
	return nil
}

// save records part of the Octavia topology on the status.
//
// Each sync step calls it twice: once on the way out, and once more if it has
// to give up half way, so the status always describes what exists in Octavia
// rather than what was planned. what names the part for the error message.
func (r *KuryrLoadBalancerReconciler) save(ctx context.Context,
	klb *kuryrv1.KuryrLoadBalancer, what string, record func()) error {

	patch := client.MergeFrom(klb.DeepCopy())
	record()
	if err := r.Status().Patch(ctx, klb, patch); err != nil {
		return fmt.Errorf("recording %s on KuryrLoadBalancer %s/%s: %w",
			what, klb.Namespace, klb.Name, err)
	}
	return nil
}

// fail records why the load balancer is not ready. It is best effort: the
// caller returns the real error either way.
func (r *KuryrLoadBalancerReconciler) fail(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer,
	reason string, cause error) {

	if err := r.setCondition(ctx, klb, metav1.ConditionFalse, reason, cause.Error()); err != nil {
		log.FromContext(ctx).V(1).Info("could not record the failure condition",
			"reason", reason, "cause", cause, "error", err)
	}
}

func (r *KuryrLoadBalancerReconciler) setCondition(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer,
	status metav1.ConditionStatus, reason, message string) error {

	patch := client.MergeFrom(klb.DeepCopy())
	if !meta.SetStatusCondition(&klb.Status.Conditions, metav1.Condition{
		Type:    kuryrv1.KuryrLoadBalancerConditionReady,
		Status:  status,
		Reason:  reason,
		Message: truncateMessage(message),
	}) {
		return nil
	}
	if err := r.Status().Patch(ctx, klb, patch); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("updating the Ready condition on KuryrLoadBalancer %s/%s: %w",
			klb.Namespace, klb.Name, err)
	}
	return nil
}

// SetupWithManager registers the reconciler.
func (r *KuryrLoadBalancerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuryrv1.KuryrLoadBalancer{}).
		// The VIP rules mirror the rules of the backends' policy groups, so a
		// policy whose rules change must refresh them, as Python's
		// update_lbaas_sg does from the KuryrNetworkPolicy handler.
		Watches(&kuryrv1.KuryrNetworkPolicy{}, handler.EnqueueRequestsFromMapFunc(r.loadBalancersInNamespace)).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.Config.Kubernetes.MaxConcurrentReconciles,
		}).
		Named("kuryrloadbalancer").
		Complete(r)
}

// loadBalancersInNamespace maps an object onto every KuryrLoadBalancer in its
// namespace.
func (r *KuryrLoadBalancerReconciler) loadBalancersInNamespace(ctx context.Context,
	obj client.Object) []reconcile.Request {

	var list kuryrv1.KuryrLoadBalancerList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing KuryrLoadBalancers to refresh their VIP rules",
			"namespace", obj.GetNamespace())
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return out
}

// --- small helpers -----------------------------------------------------

func listenerKey(protocol string, port int32) string {
	return fmt.Sprintf("%s/%d", protocol, port)
}

func memberKey(poolID, ip string, port int32) string {
	return fmt.Sprintf("%s/%s/%d", poolID, ip, port)
}

// portFor returns the service port a listener was created for.
func portFor(ports []kuryrv1.ServicePort, l kuryrv1.Listener) (kuryrv1.ServicePort, bool) {
	for _, p := range ports {
		if p.Port == l.Port && p.Protocol == l.Protocol {
			return p, true
		}
	}
	return kuryrv1.ServicePort{}, false
}

// backendPort returns the port the backends serve for one service port.
//
// EndpointSlice names its ports after the Service port, so the name is the
// join key. A Service with a single unnamed port produces an unnamed slice
// port, and the empty names match.
func backendPort(subset *kuryrv1.EndpointSubset, servicePort kuryrv1.ServicePort) (int32, bool) {
	for _, p := range subset.Ports {
		if p.Name == servicePort.Name && p.Protocol == servicePort.Protocol {
			return p.Port, true
		}
	}
	return 0, false
}
