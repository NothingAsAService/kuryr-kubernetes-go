package policy

import (
	"context"
	"fmt"
	"net/netip"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// ServiceRef is one Service whose cluster IP a policy may have to allow.
type ServiceRef struct {
	Namespace string
	Name      string
	// ClusterIPs are the Service's virtual addresses, one per address family.
	ClusterIPs []string
	// Selector picks the Service's backends. Empty for a Service whose
	// endpoints are managed by hand.
	Selector map[string]string
}

// Options are the settings the translation reads from kuryr.conf.
type Options struct {
	// ServiceIngress allows ingress from every cluster IP in the policy's
	// namespace. Python does this when [octavia_defaults]enforce_sg_rules is
	// off: OVN load balancers rewrite the source of hairpin traffic (a pod
	// reaching itself through its own Service) to the Service IP. With
	// enforce_sg_rules on, a default rule already opens the service subnet.
	ServiceIngress bool
}

// serviceIngressRules allows ingress from every Service cluster IP in the
// policy's namespace.
func serviceIngressRules(ctx context.Context, np *netv1.NetworkPolicy, r Resolver) ([]kuryrv1.SGRuleSpec, error) {
	services, err := r.Services(ctx, np.Namespace)
	if err != nil {
		return nil, fmt.Errorf("listing Services for hairpin ingress: %w", err)
	}
	var out []kuryrv1.SGRuleSpec
	for _, svc := range services {
		for _, ip := range svc.ClusterIPs {
			rules, err := clusterIPRules(kuryrv1.SGDirectionIngress, ip, []portRule{{}}, np)
			if err != nil {
				return nil, err
			}
			out = append(out, rules...)
		}
	}
	return out, nil
}

// serviceEgressRules allows egress to the cluster IPs of the Services that
// front a peer the policy already allows, on the same ports, as Python's
// _create_svc_egress_sg_rule does. Traffic to a Service leaves the pod
// addressed to the cluster IP, so allowing only the backends' own addresses
// would block exactly the path most clients use.
//
// Matching follows Python: a pod peer is fronted by the Services in its
// namespace whose selector matches its labels; a namespace peer by every
// Service in that namespace; an ipBlock peer by the policy-namespace Services
// whose selected pods fall inside the block. The "anywhere" peer already
// covers every cluster IP.
//
// shortcut: Services without a selector (hand-managed endpoints) are
// skipped; Python matches them by endpoint targets. Upgrade if such Services
// must be reachable through egress policies.
func serviceEgressRules(ctx context.Context, pr peer, ports []portRule, np *netv1.NetworkPolicy,
	r Resolver) ([]kuryrv1.SGRuleSpec, error) {

	if pr.CIDR == allIPv4 || pr.CIDR == allIPv6 {
		return nil, nil
	}

	var namespace string
	switch {
	case pr.Pod != nil:
		namespace = pr.Pod.Namespace
	case pr.Namespace != "":
		namespace = pr.Namespace
	default:
		namespace = np.Namespace
	}
	services, err := r.Services(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("listing Services in namespace %s for egress rules: %w", namespace, err)
	}

	var out []kuryrv1.SGRuleSpec
	for _, svc := range services {
		if len(svc.Selector) == 0 {
			continue
		}
		fronts, err := svcFronts(ctx, svc, pr, r)
		if err != nil {
			return nil, err
		}
		if !fronts {
			continue
		}
		for _, ip := range svc.ClusterIPs {
			rules, err := clusterIPRules(kuryrv1.SGDirectionEgress, ip, ports, np)
			if err != nil {
				return nil, err
			}
			out = append(out, rules...)
		}
	}
	return out, nil
}

// svcFronts reports whether svc sends traffic to the peer.
func svcFronts(ctx context.Context, svc ServiceRef, pr peer, r Resolver) (bool, error) {
	selector := labels.SelectorFromSet(svc.Selector)
	switch {
	case pr.Pod != nil:
		return selector.Matches(labels.Set(pr.Pod.Labels)), nil
	case pr.Namespace != "":
		return true, nil
	}

	block, err := netip.ParsePrefix(pr.CIDR)
	if err != nil {
		return false, err
	}
	pods, err := r.Pods(ctx, labelSelectorFor(svc.Selector), []string{svc.Namespace})
	if err != nil {
		return false, err
	}
	for _, pod := range pods {
		if ip, err := netip.ParseAddr(pod.IP); err == nil && block.Contains(ip) {
			return true, nil
		}
	}
	return false, nil
}

// clusterIPRules writes rules for one cluster IP, with no pod or namespace
// attached: the address is the Service's, not a peer's.
func clusterIPRules(direction kuryrv1.SGDirection, ip string, ports []portRule,
	np *netv1.NetworkPolicy) ([]kuryrv1.SGRuleSpec, error) {

	cidr, err := HostCIDR(ip)
	if err != nil {
		return nil, fmt.Errorf("service cluster IP %q: %w", ip, err)
	}
	return rulesFor(direction, peer{CIDR: cidr}, ports, np)
}

func labelSelectorFor(set map[string]string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: set}
}
