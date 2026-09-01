package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	"opendev.org/openstack/kuryr-kubernetes/pkg/policy"
)

// KuryrNetworkPolicyReconciler makes Neutron match a translated policy.
//
// The security group is created once and then kept in step rule by rule.
// Rules are never rewritten wholesale, because a security group applied to
// live ports enforces whatever it contains at that moment: deleting every
// rule and recreating it would briefly drop traffic the policy allows.
type KuryrNetworkPolicyReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Config    *config.Config
	OpenStack securityGroupCloud

	// Project resolves which OpenStack project a policy security group belongs
	// in, for namespaces that have no KuryrNetwork to read it from.
	Project drivers.ProjectDriver
}

// securityGroupCloud is the slice of Neutron this reconciler uses. Naming it
// as an interface rather than taking *openstack.Client is what lets the rule
// diffing, which is the part worth getting right, be tested without a cloud.
type securityGroupCloud interface {
	CreateSecurityGroup(ctx context.Context, name, projectID, description string) (*openstack.SecurityGroup, error)
	FindSecurityGroupsByName(ctx context.Context, name, projectID string) ([]*openstack.SecurityGroup, error)
	DeleteSecurityGroup(ctx context.Context, id string) error
	CreateSGRule(ctx context.Context, r openstack.SGRule) (*openstack.SGRule, error)
	DeleteSGRule(ctx context.Context, id string) error
	GetSubnet(ctx context.Context, id string) (*openstack.Subnet, error)
}

var _ securityGroupCloud = (*openstack.Client)(nil)

// +kubebuilder:rbac:groups=openstack.org,resources=kuryrnetworkpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrnetworkpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrnetworkpolicies/finalizers,verbs=update

// Reconcile creates the security group for a policy and keeps its rules
// current.
func (r *KuryrNetworkPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var knp kuryrv1.KuryrNetworkPolicy
	if err := r.Get(ctx, req.NamespacedName, &knp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading KuryrNetworkPolicy %s: %w", req.NamespacedName, err)
	}

	if !knp.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &knp)
	}

	if !controllerutil.ContainsFinalizer(&knp, kuryrv1.KuryrNetworkPolicyFinalizer) {
		// The security group is about to exist, so the finalizer has to be in
		// place before it does. Without it a delete would leave the group
		// behind with no record of it.
		patch := client.MergeFrom(knp.DeepCopy())
		controllerutil.AddFinalizer(&knp, kuryrv1.KuryrNetworkPolicyFinalizer)
		if err := r.Patch(ctx, &knp, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer to KuryrNetworkPolicy %s: %w",
				req.NamespacedName, err)
		}
	}

	if err := r.ensureSecurityGroup(ctx, &knp); err != nil {
		r.fail(ctx, &knp, "SecurityGroupFailed", err)
		return ctrl.Result{}, err
	}

	if err := r.syncRules(ctx, &knp); err != nil {
		r.fail(ctx, &knp, "RulesFailed", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.markEnforced(ctx, &knp)
}

// ensureSecurityGroup creates the Neutron group backing the policy, once.
func (r *KuryrNetworkPolicyReconciler) ensureSecurityGroup(ctx context.Context, knp *kuryrv1.KuryrNetworkPolicy) error {
	if knp.Status.SecurityGroupID != "" {
		return nil
	}

	projectID, err := r.projectFor(ctx, knp)
	if err != nil {
		return err
	}

	name := policySecurityGroupName(knp.Namespace, knp.Name, string(knp.UID))
	description := fmt.Sprintf("kuryr: NetworkPolicy %s/%s", knp.Namespace, knp.Name)

	// A previous pass may have created the group and then failed before
	// recording its ID. Adopting it keeps a retry from leaking a second one.
	existing, err := r.OpenStack.FindSecurityGroupsByName(ctx, name, projectID)
	if err != nil {
		return fmt.Errorf("looking for an existing security group named %q: %w", name, err)
	}

	var sgID string
	switch len(existing) {
	case 0:
		sg, err := r.OpenStack.CreateSecurityGroup(ctx, name, projectID, description)
		if err != nil {
			return fmt.Errorf("creating security group for NetworkPolicy %s/%s: %w",
				knp.Namespace, knp.Name, err)
		}
		sgID = sg.ID
	case 1:
		sgID = existing[0].ID
		log.FromContext(ctx).Info("adopted an existing security group", "securityGroupID", sgID)
	default:
		// The name carries the policy UID, so duplicates cannot come from two
		// different policies. Picking one arbitrarily would leak the others
		// silently, so stop and let an operator look.
		return fmt.Errorf("found %d security groups named %q; expected at most one, "+
			"remove the duplicates before continuing", len(existing), name)
	}

	patch := client.MergeFrom(knp.DeepCopy())
	knp.Status.SecurityGroupID = sgID
	if err := r.Status().Patch(ctx, knp, patch); err != nil {
		// The group exists but is unrecorded. The lookup above adopts it on
		// the next pass, so report the failure and let that happen.
		return fmt.Errorf("recording security group %s on KuryrNetworkPolicy %s/%s: %w",
			sgID, knp.Namespace, knp.Name, err)
	}

	log.FromContext(ctx).Info("security group ready", "securityGroupID", sgID)
	return nil
}

// projectFor returns the OpenStack project the group belongs to.
//
// It follows the namespace's KuryrNetwork when namespaces have their own
// networks, because the group has to live alongside the ports it is applied
// to. Otherwise everything shares the configured default project.
func (r *KuryrNetworkPolicyReconciler) projectFor(ctx context.Context,
	knp *kuryrv1.KuryrNetworkPolicy) (string, error) {

	var list kuryrv1.KuryrNetworkList
	if err := r.List(ctx, &list, client.InNamespace(knp.Namespace)); err != nil {
		return "", fmt.Errorf("listing KuryrNetworks in namespace %s: %w", knp.Namespace, err)
	}
	for i := range list.Items {
		if id := list.Items[i].Spec.ProjectID; id != "" {
			return id, nil
		}
	}

	// No KuryrNetwork, so this namespace has no network of its own. Ask the
	// configured driver, which is what honours a namespace annotation when
	// [kubernetes]network_policy_project_driver is set to annotation.
	//
	// The KuryrNetwork is preferred over the driver rather than the other way
	// round: it names the project the namespace's network was actually built
	// in, so putting the policy's security group anywhere else would leave it
	// unable to apply to the ports it is meant to govern.
	if r.Project != nil {
		ns := &corev1.Namespace{}
		ns.Name = knp.Namespace
		id, err := r.Project.ProjectFor(ctx, ns)
		if err != nil {
			return "", fmt.Errorf("resolving the project of namespace %s: %w", knp.Namespace, err)
		}
		if id != "" {
			return id, nil
		}
	}

	if r.Config.NeutronDefaults.Project == "" {
		return "", fmt.Errorf("no project for namespace %s: neither a KuryrNetwork, the "+
			"network policy project driver, nor [neutron_defaults]project provided one",
			knp.Namespace)
	}
	return r.Config.NeutronDefaults.Project, nil
}

// syncRules makes the group's rules match the spec.
//
// Rules are compared by content, so a rule that is unchanged is left exactly
// as it is. Additions go in before removals: widening the group before
// narrowing it never drops traffic the new policy still allows.
func (r *KuryrNetworkPolicyReconciler) syncRules(ctx context.Context, knp *kuryrv1.KuryrNetworkPolicy) error {
	logger := log.FromContext(ctx)
	sgID := knp.Status.SecurityGroupID

	infrastructure, err := r.infrastructureRules(ctx)
	if err != nil {
		return err
	}

	desired := make([]kuryrv1.SGRuleSpec, 0,
		len(knp.Spec.IngressRules)+len(knp.Spec.EgressRules)+len(infrastructure))
	desired = append(desired, knp.Spec.IngressRules...)
	desired = append(desired, knp.Spec.EgressRules...)
	desired = append(desired, infrastructure...)

	// Index what exists by content so equal rules match up regardless of the
	// order either list happens to be in.
	have := make(map[string]kuryrv1.CreatedSGRule, len(knp.Status.SecurityGroupRules))
	for _, created := range knp.Status.SecurityGroupRules {
		have[ruleKey(created.Rule)] = created
	}

	kept := make(map[string]bool, len(desired))
	next := make([]kuryrv1.CreatedSGRule, 0, len(desired))
	created := 0

	for _, want := range desired {
		key := ruleKey(want.Rule)
		if kept[key] {
			// Two peers can expand to the same rule; one is enough.
			continue
		}
		kept[key] = true

		if existing, ok := have[key]; ok {
			next = append(next, existing)
			continue
		}

		rule, err := r.OpenStack.CreateSGRule(ctx, neutronRule(sgID, want.Rule))
		if err != nil {
			// Stop here rather than pressing on: the rules already created
			// are recorded below, so the retry picks up where this left off
			// instead of trying the whole set again.
			if saveErr := r.saveRules(ctx, knp, next); saveErr != nil {
				return fmt.Errorf("%w (and recording the rules created so far failed: %w)", err, saveErr)
			}
			return err
		}
		next = append(next, kuryrv1.CreatedSGRule{
			ID:              rule.ID,
			SecurityGroupID: sgID,
			Rule:            want.Rule,
		})
		created++
	}

	// Whatever is no longer wanted goes now that every new rule is in place.
	var removed int
	for key, existing := range have {
		if kept[key] {
			continue
		}
		if err := r.OpenStack.DeleteSGRule(ctx, existing.ID); err != nil {
			if !openstack.IsNotFound(err) {
				if saveErr := r.saveRules(ctx, knp, next); saveErr != nil {
					return fmt.Errorf("%w (and recording the surviving rules failed: %w)", err, saveErr)
				}
				return fmt.Errorf("deleting security group rule %s: %w", existing.ID, err)
			}
			// Already gone in Neutron, which is the state we wanted.
		}
		removed++
	}

	if created == 0 && removed == 0 && len(next) == len(knp.Status.SecurityGroupRules) {
		return nil
	}

	if err := r.saveRules(ctx, knp, next); err != nil {
		return err
	}
	logger.Info("security group rules synced",
		"securityGroupID", sgID, "created", created, "removed", removed, "total", len(next))
	return nil
}

// infrastructureRules are the ingress rules every policy security group
// needs regardless of what the policy says.
//
// A pod isolated by a policy still has to be reachable by kubelet, or its
// liveness and readiness probes fail and Kubernetes restarts it in a loop.
// The same goes for load balancer traffic when Octavia enforces security
// groups on the members. Neither is expressible as a NetworkPolicy, so the
// rules come from configuration.
func (r *KuryrNetworkPolicyReconciler) infrastructureRules(ctx context.Context) ([]kuryrv1.SGRuleSpec, error) {
	subnetIDs := make([]string, 0, len(r.Config.PodVIFNested.WorkerNodesSubnets)+1)
	subnetIDs = append(subnetIDs, r.Config.PodVIFNested.WorkerNodesSubnets...)
	if r.Config.OctaviaDefaults.EnforceSGRules && r.Config.NeutronDefaults.ServiceSubnet != "" {
		subnetIDs = append(subnetIDs, r.Config.NeutronDefaults.ServiceSubnet)
	}

	out := make([]kuryrv1.SGRuleSpec, 0, len(subnetIDs))
	for _, id := range subnetIDs {
		subnet, err := r.OpenStack.GetSubnet(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("reading subnet %s to build the policy base rules: %w", id, err)
		}
		if subnet.CIDR == "" {
			return nil, fmt.Errorf("subnet %s has no CIDR, cannot build the policy base rules", id)
		}

		etherType, err := policy.EtherTypeOf(subnet.CIDR)
		if err != nil {
			return nil, fmt.Errorf("subnet %s: %w", id, err)
		}

		out = append(out, kuryrv1.SGRuleSpec{
			Rule: kuryrv1.SGRule{
				Direction:      kuryrv1.SGDirectionIngress,
				EtherType:      kuryrv1.SGEtherType(etherType),
				RemoteIPPrefix: subnet.CIDR,
				Description:    "kuryr: infrastructure access, required for probes and services",
			},
		})
	}
	return out, nil
}

// saveRules records the rules that currently exist in Neutron.
func (r *KuryrNetworkPolicyReconciler) saveRules(ctx context.Context, knp *kuryrv1.KuryrNetworkPolicy,
	rules []kuryrv1.CreatedSGRule) error {

	patch := client.MergeFrom(knp.DeepCopy())
	knp.Status.SecurityGroupRules = rules
	knp.Status.PodSelector = knp.Spec.PodSelector.DeepCopy()
	if err := r.Status().Patch(ctx, knp, patch); err != nil {
		return fmt.Errorf("recording security group rules on KuryrNetworkPolicy %s/%s: %w",
			knp.Namespace, knp.Name, err)
	}
	return nil
}

// finalize removes the security group once the policy is gone.
func (r *KuryrNetworkPolicyReconciler) finalize(ctx context.Context,
	knp *kuryrv1.KuryrNetworkPolicy) (ctrl.Result, error) {

	if !controllerutil.ContainsFinalizer(knp, kuryrv1.KuryrNetworkPolicyFinalizer) {
		return ctrl.Result{}, nil
	}

	if sgID := knp.Status.SecurityGroupID; sgID != "" {
		// Neutron refuses to delete a group still applied to a port. That
		// means a pod has not been updated yet, so retry rather than force
		// it: the KuryrPort controller removes the group as the policy stops
		// selecting the pod.
		if err := r.OpenStack.DeleteSecurityGroup(ctx, sgID); err != nil {
			if openstack.IsConflict(err) {
				log.FromContext(ctx).V(1).Info(
					"security group is still applied to ports, retrying", "securityGroupID", sgID)
				return ctrl.Result{RequeueAfter: sgDeleteRequeue}, nil
			}
			if !openstack.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("deleting security group %s: %w", sgID, err)
			}
			// Already gone, which is the state we wanted.
		}
		log.FromContext(ctx).Info("security group deleted", "securityGroupID", sgID)
	}

	patch := client.MergeFrom(knp.DeepCopy())
	controllerutil.RemoveFinalizer(knp, kuryrv1.KuryrNetworkPolicyFinalizer)
	if err := r.Patch(ctx, knp, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("removing finalizer from KuryrNetworkPolicy %s/%s: %w",
			knp.Namespace, knp.Name, err)
	}
	return ctrl.Result{}, nil
}

// sgDeleteRequeue is how long to wait before retrying a group that is still
// in use. Ports are updated by another controller, so this is a short wait on
// work already in progress.
const sgDeleteRequeue = 5 * time.Second

func (r *KuryrNetworkPolicyReconciler) markEnforced(ctx context.Context, knp *kuryrv1.KuryrNetworkPolicy) error {
	return r.setCondition(ctx, knp, metav1.ConditionTrue, "Enforced",
		fmt.Sprintf("security group %s has %d rules",
			knp.Status.SecurityGroupID, len(knp.Status.SecurityGroupRules)))
}

// fail records why the policy is not enforced.
//
// The condition is best effort: the caller returns the real error either way,
// so a failure to write the condition must not hide the original cause.
func (r *KuryrNetworkPolicyReconciler) fail(ctx context.Context, knp *kuryrv1.KuryrNetworkPolicy,
	reason string, cause error) {

	if err := r.setCondition(ctx, knp, metav1.ConditionFalse, reason, cause.Error()); err != nil {
		log.FromContext(ctx).V(1).Info("could not record the failure condition",
			"reason", reason, "cause", cause, "error", err)
	}
}

func (r *KuryrNetworkPolicyReconciler) setCondition(ctx context.Context, knp *kuryrv1.KuryrNetworkPolicy,
	status metav1.ConditionStatus, reason, message string) error {

	patch := client.MergeFrom(knp.DeepCopy())
	if !meta.SetStatusCondition(&knp.Status.Conditions, metav1.Condition{
		Type:    kuryrv1.KuryrNetworkPolicyConditionEnforced,
		Status:  status,
		Reason:  reason,
		Message: truncateMessage(message),
	}) {
		return nil
	}
	if err := r.Status().Patch(ctx, knp, patch); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// The object moved on. The next pass writes the condition.
			return nil
		}
		return fmt.Errorf("updating the Enforced condition on KuryrNetworkPolicy %s/%s: %w",
			knp.Namespace, knp.Name, err)
	}
	return nil
}

// ruleKey identifies a rule by what it allows, which is what makes two rules
// the same for our purposes. The description is left out: it is provenance,
// not behaviour, and including it would recreate every rule when a policy is
// renamed.
func ruleKey(r kuryrv1.SGRule) string {
	return strings.Join([]string{
		string(r.Direction),
		string(r.EtherType),
		r.Protocol,
		fmt.Sprint(r.PortRangeMin),
		fmt.Sprint(r.PortRangeMax),
		r.RemoteIPPrefix,
	}, "|")
}

// neutronRule converts a stored rule to the form the cloud client takes.
func neutronRule(sgID string, r kuryrv1.SGRule) openstack.SGRule {
	return openstack.SGRule{
		SecurityGroupID: sgID,
		Direction:       string(r.Direction),
		EtherType:       string(r.EtherType),
		Protocol:        r.Protocol,
		PortRangeMin:    int(r.PortRangeMin),
		PortRangeMax:    int(r.PortRangeMax),
		RemoteIPPrefix:  r.RemoteIPPrefix,
		Description:     r.Description,
	}
}

// SetupWithManager registers the reconciler.
func (r *KuryrNetworkPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuryrv1.KuryrNetworkPolicy{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.Config.Kubernetes.MaxConcurrentReconciles,
		}).
		Named("kuryrnetworkpolicy").
		Complete(r)
}
