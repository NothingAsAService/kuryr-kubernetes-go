package drivers

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

func init() {
	RegisterSecurityGroupsDriver("policy", newPolicySGDriver)
}

// policySGDriver picks the security groups for a pod from the
// NetworkPolicies that select it.
//
// A pod no policy selects is unrestricted and gets the configured default
// groups. A pod some policy selects gets those policies' groups, and a
// security group denies whatever it does not allow, which is exactly the
// isolation NetworkPolicy asks for.
//
// The subtlety is the direction nothing restricts. Kubernetes decides that
// per pod: a pod's egress is restricted only if some policy selecting it
// lists Egress. So when none of the matching policies governs a direction,
// this driver adds a shared group that opens it. Deciding that here, rather
// than inside each policy's own group, is what stops one permissive policy
// from widening a restrictive one that happens to select the same pod --
// Neutron unions a port's groups, so a group opened for one policy's sake
// would apply to every pod carrying it.
type policySGDriver struct {
	k8s      client.Client
	defaults []string
	allowAll *allowAllGroups
}

func newPolicySGDriver(d Deps) (SecurityGroupsDriver, error) {
	if d.K8s == nil {
		return nil, fmt.Errorf("the policy security groups driver needs a Kubernetes client")
	}
	if d.OpenStack == nil {
		return nil, fmt.Errorf("the policy security groups driver needs an OpenStack client")
	}
	if len(d.Config.NeutronDefaults.PodSecurityGroups) == 0 {
		// Without this, a pod selected by no policy would end up with no
		// security group at all, which Neutron reads as "the project default
		// group". That is not something to leave to chance.
		return nil, fmt.Errorf("the policy security groups driver requires " +
			"[neutron_defaults] pod_security_groups, used for pods no NetworkPolicy selects")
	}
	return &policySGDriver{
		k8s:      d.K8s,
		defaults: d.Config.NeutronDefaults.PodSecurityGroups,
		allowAll: newAllowAllGroups(d.OpenStack),
	}, nil
}

// SecurityGroupsFor returns the groups to apply to a pod's port.
func (s *policySGDriver) SecurityGroupsFor(ctx context.Context, obj client.Object,
	projectID string) ([]string, error) {

	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// Services are handled by their own driver; anything else reaching
		// here is a wiring mistake worth reporting rather than defaulting.
		return nil, fmt.Errorf("the policy security groups driver expects a Pod, got %T", obj)
	}

	var list kuryrv1.KuryrNetworkPolicyList
	if err := s.k8s.List(ctx, &list, client.InNamespace(pod.Namespace)); err != nil {
		return nil, fmt.Errorf("listing KuryrNetworkPolicies in namespace %s: %w", pod.Namespace, err)
	}

	podLabels := labels.Set(pod.Labels)
	seen := map[string]bool{}
	var groups []string
	restricted := map[kuryrv1.SGDirection]bool{}

	for i := range list.Items {
		knp := &list.Items[i]
		if !knp.DeletionTimestamp.IsZero() {
			// The policy is going away, so it no longer isolates this pod.
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(&knp.Spec.PodSelector)
		if err != nil {
			return nil, fmt.Errorf("podSelector of KuryrNetworkPolicy %s/%s: %w",
				knp.Namespace, knp.Name, err)
		}
		if !selector.Matches(podLabels) {
			continue
		}

		if knp.Status.SecurityGroupID == "" {
			// The policy applies but its group does not exist yet. Falling
			// back to the defaults would leave the pod unrestricted for as
			// long as that took, so wait instead.
			return nil, fmt.Errorf("%w: security group for NetworkPolicy %s/%s is not created yet",
				ErrRetryLater, knp.Namespace, knp.Name)
		}
		if !seen[knp.Status.SecurityGroupID] {
			seen[knp.Status.SecurityGroupID] = true
			groups = append(groups, knp.Status.SecurityGroupID)
		}

		for _, d := range governedDirections(knp) {
			restricted[d] = true
		}
	}

	if len(groups) == 0 {
		return append([]string(nil), s.defaults...), nil
	}

	for _, direction := range []kuryrv1.SGDirection{kuryrv1.SGDirectionIngress, kuryrv1.SGDirectionEgress} {
		if restricted[direction] {
			continue
		}
		id, err := s.allowAll.id(ctx, projectID, direction)
		if err != nil {
			return nil, fmt.Errorf("no policy restricts %s for pod %s/%s, so it needs the "+
				"shared allow-all group: %w", direction, pod.Namespace, pod.Name, err)
		}
		if !seen[id] {
			seen[id] = true
			groups = append(groups, id)
		}
	}

	// A stable order keeps the port update a no-op when nothing changed.
	slices.Sort(groups)
	return groups, nil
}

// governedDirections returns the directions a policy actually restricts.
//
// An empty policyTypes should not happen -- the translation always fills it,
// and Kubernetes defaults it to Ingress when the user leaves it out -- but
// reading it as "restricts nothing" would hand the pod an allow-all group on
// the strength of a missing field. Ingress is the safe reading, and it is
// also what Kubernetes itself defaults to.
func governedDirections(knp *kuryrv1.KuryrNetworkPolicy) []kuryrv1.SGDirection {
	if len(knp.Spec.PolicyTypes) == 0 {
		return []kuryrv1.SGDirection{kuryrv1.SGDirectionIngress}
	}

	out := make([]kuryrv1.SGDirection, 0, len(knp.Spec.PolicyTypes))
	for _, t := range knp.Spec.PolicyTypes {
		switch netv1.PolicyType(t) {
		case netv1.PolicyTypeIngress:
			out = append(out, kuryrv1.SGDirectionIngress)
		case netv1.PolicyTypeEgress:
			out = append(out, kuryrv1.SGDirectionEgress)
		}
	}
	return out
}
