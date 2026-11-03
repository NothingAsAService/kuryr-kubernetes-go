package policy

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// Everything matches these when a peer list is empty, which NetworkPolicy
// defines as "from anywhere".
const (
	allIPv4 = "0.0.0.0/0"
	allIPv6 = "::/0"

	// protoTCP is both the default when a NetworkPolicy port names no
	// protocol and the fallback for one Neutron does not model.
	protoTCP = "tcp"
)

// Resolver answers the cluster questions the translation needs. Keeping it an
// interface is what lets the translation be tested exhaustively without a
// cluster.
type Resolver interface {
	// Namespaces returns the names of namespaces matching sel. A nil sel
	// means "the policy's own namespace only".
	Namespaces(ctx context.Context, sel *metav1.LabelSelector, policyNamespace string) ([]string, error)

	// Pods returns the pods matching sel across namespaces. A nil sel matches
	// every pod in those namespaces.
	Pods(ctx context.Context, sel *metav1.LabelSelector, namespaces []string) ([]PodRef, error)

	// NamespaceCIDR returns the subnet CIDR backing a namespace, or an empty
	// string when namespaces do not have their own subnets.
	NamespaceCIDR(ctx context.Context, namespace string) (string, error)

	// NamedPorts resolves a named container port to the numbers it takes on
	// the pods the policy applies to. It answers for ingress rules only; for
	// egress the name belongs to the peer, and PodRef.ContainerPorts carries
	// it instead.
	NamedPorts(ctx context.Context, sel *metav1.LabelSelector, namespace, portName string) ([]int32, error)

	// Services returns the Services in a namespace that have a cluster IP
	// and are not being deleted.
	Services(ctx context.Context, namespace string) ([]ServiceRef, error)
}

// PodRef is one pod a peer selector matched.
type PodRef struct {
	// Namespace and Name identify the pod, for diagnostics.
	Namespace string
	Name      string
	// IP is the address the rule is written against.
	IP string
	// ContainerPorts maps each named container port to the numbers it takes
	// on this pod.
	//
	// It is here because of egress. A named port in an ingress rule names a
	// port on the pods the policy selects, which is one lookup for the whole
	// policy; in an egress rule it names a port on the *destination*, so the
	// number depends on which peer pod the rule is being written for. Two
	// backends can map the same name to different numbers, and resolving
	// against the policy's own pods -- as this package used to, and as
	// upstream kuryr-kubernetes's ingress path does for both directions -- opens the
	// wrong port or none at all.
	ContainerPorts map[string][]int32
	// Labels are the pod's labels, which decide the Services fronting it.
	Labels map[string]string
}

// affected turns a matched pod into the CRD's record of it.
func (p PodRef) affected() kuryrv1.AffectedPod {
	return kuryrv1.AffectedPod{PodIP: p.IP, PodNamespace: p.Namespace}
}

// Translate turns a NetworkPolicy into the security group rules that enforce
// it.
//
// A security group denies everything it does not allow, which lines up with
// NetworkPolicy: selecting a pod for a direction denies that direction unless
// a rule permits it. So "deny all ingress" needs no rule at all, and the
// absence of rules here is meaningful rather than a failure.
func Translate(ctx context.Context, np *netv1.NetworkPolicy, r Resolver,
	opts Options) (kuryrv1.KuryrNetworkPolicySpec, error) {

	spec := kuryrv1.KuryrNetworkPolicySpec{
		PodSelector: np.Spec.PodSelector,
		PolicyTypes: policyTypes(np),
	}

	for _, t := range spec.PolicyTypes {
		switch netv1.PolicyType(t) {
		case netv1.PolicyTypeIngress:
			rules, err := translateIngress(ctx, np, r)
			if err == nil && opts.ServiceIngress {
				var hairpin []kuryrv1.SGRuleSpec
				hairpin, err = serviceIngressRules(ctx, np, r)
				rules = append(rules, hairpin...)
			}
			if err != nil {
				return spec, fmt.Errorf("translating ingress rules of NetworkPolicy %s/%s: %w",
					np.Namespace, np.Name, err)
			}
			spec.IngressRules = rules

		case netv1.PolicyTypeEgress:
			rules, err := translateEgress(ctx, np, r)
			if err != nil {
				return spec, fmt.Errorf("translating egress rules of NetworkPolicy %s/%s: %w",
					np.Namespace, np.Name, err)
			}
			spec.EgressRules = rules
		}
	}

	// A direction the policy does not govern is deliberately left with no
	// rules at all.
	//
	// It is tempting to open it here instead: a security group denies what it
	// does not allow, so an empty direction blocks traffic this policy says
	// nothing about. That is what upstream kuryr-kubernetes does, and it is wrong.
	// Kubernetes decides isolation per pod, not per policy: a pod's ingress
	// is restricted only if *some* policy selecting it lists Ingress. Putting
	// allow-everything rules in this policy's own group would open that
	// direction for every pod carrying the group -- including pods that also
	// carry a second policy which does restrict it, because Neutron unions a
	// port's groups. One egress-only policy would silently undo every ingress
	// rule of every other policy on the same pod.
	//
	// The decision therefore belongs where the whole set of policies on a pod
	// is visible, which is the pod security groups driver in
	// pkg/drivers/policy_sg.go.
	return spec, nil
}

// policyTypes returns the directions a policy governs.
//
// When spec.policyTypes is omitted, Kubernetes infers it: Ingress always, and
// Egress only if egress rules are present.
func policyTypes(np *netv1.NetworkPolicy) []string {
	if len(np.Spec.PolicyTypes) > 0 {
		out := make([]string, 0, len(np.Spec.PolicyTypes))
		for _, t := range np.Spec.PolicyTypes {
			out = append(out, string(t))
		}
		return out
	}

	out := []string{string(netv1.PolicyTypeIngress)}
	if len(np.Spec.Egress) > 0 {
		out = append(out, string(netv1.PolicyTypeEgress))
	}
	return out
}

func translateIngress(ctx context.Context, np *netv1.NetworkPolicy, r Resolver) ([]kuryrv1.SGRuleSpec, error) {
	var out []kuryrv1.SGRuleSpec

	for i, rule := range np.Spec.Ingress {
		// An ingress named port belongs to the pods the policy selects, so it
		// is the same for every peer and is resolved once.
		peers, err := resolvePeers(ctx, rule.From, np.Namespace, r, false)
		if err != nil {
			return nil, fmt.Errorf("ingress rule %d: %w", i, err)
		}
		ports, err := ingressPorts(ctx, rule.Ports, np, r)
		if err != nil {
			return nil, fmt.Errorf("ingress rule %d: %w", i, err)
		}

		for _, pr := range peers {
			rules, err := rulesFor(kuryrv1.SGDirectionIngress, pr, ports, np)
			if err != nil {
				return nil, fmt.Errorf("ingress rule %d: %w", i, err)
			}
			out = append(out, rules...)
		}
	}

	return out, nil
}

func translateEgress(ctx context.Context, np *netv1.NetworkPolicy, r Resolver) ([]kuryrv1.SGRuleSpec, error) {
	var out []kuryrv1.SGRuleSpec

	for i, rule := range np.Spec.Egress {
		// A named port here belongs to the destination, so the peers have to
		// be resolved down to individual pods even when a whole namespace
		// could otherwise have collapsed into a single subnet rule.
		peers, err := resolvePeers(ctx, rule.To, np.Namespace, r, hasNamedPort(rule.Ports))
		if err != nil {
			return nil, fmt.Errorf("egress rule %d: %w", i, err)
		}

		for _, pr := range peers {
			ports, err := egressPorts(ctx, rule.Ports, pr, np)
			if err != nil {
				return nil, fmt.Errorf("egress rule %d: %w", i, err)
			}
			rules, err := rulesFor(kuryrv1.SGDirectionEgress, pr, ports, np)
			if err != nil {
				return nil, fmt.Errorf("egress rule %d: %w", i, err)
			}
			out = append(out, rules...)

			viaService, err := serviceEgressRules(ctx, pr, ports, np, r)
			if err != nil {
				return nil, fmt.Errorf("egress rule %d: %w", i, err)
			}
			out = append(out, viaService...)
		}
	}

	return out, nil
}

// hasNamedPort reports whether any of the ports is given by name.
func hasNamedPort(ports []netv1.NetworkPolicyPort) bool {
	for _, p := range ports {
		if p.Port != nil && p.Port.Type == intstr.String {
			return true
		}
	}
	return false
}

// peer is one resolved traffic source or destination.
type peer struct {
	// CIDR is what the rule matches on.
	CIDR string
	// Pod records which pod this came from, when it came from one. The
	// controller uses it to recompute incrementally when a single pod moves,
	// and an egress named port is resolved against it.
	Pod *PodRef
	// Namespace scopes a rule that came from a namespaceSelector.
	Namespace string
}

// portRule is one resolved port constraint.
type portRule struct {
	Protocol string
	Min      int32
	Max      int32
}

// resolvePeers turns a NetworkPolicy peer list into CIDRs.
//
// An empty list means "anywhere", which is both address families.
//
// needPods forces every peer down to individual pods. It is set for an egress
// rule with a named port, where the port number is a property of the
// destination pod and a collapsed subnet rule would have nowhere to read it
// from.
func resolvePeers(ctx context.Context, peers []netv1.NetworkPolicyPeer, policyNamespace string,
	r Resolver, needPods bool) ([]peer, error) {

	if len(peers) == 0 {
		return []peer{{CIDR: allIPv4}, {CIDR: allIPv6}}, nil
	}

	var out []peer
	for i, p := range peers {
		resolved, err := resolvePeer(ctx, p, policyNamespace, r, needPods)
		if err != nil {
			return nil, fmt.Errorf("peer %d: %w", i, err)
		}
		out = append(out, resolved...)
	}
	return out, nil
}

func resolvePeer(ctx context.Context, p netv1.NetworkPolicyPeer, policyNamespace string,
	r Resolver, needPods bool) ([]peer, error) {

	// Exactly one of the three forms is set on a valid peer.
	if p.IPBlock != nil {
		cidrs, err := SubtractCIDRs(p.IPBlock.CIDR, p.IPBlock.Except)
		if err != nil {
			return nil, err
		}
		out := make([]peer, 0, len(cidrs))
		for _, c := range cidrs {
			out = append(out, peer{CIDR: c})
		}
		return out, nil
	}

	namespaces, err := r.Namespaces(ctx, p.NamespaceSelector, policyNamespace)
	if err != nil {
		return nil, err
	}
	if len(namespaces) == 0 {
		// The selector matched nothing, so the peer contributes no rules.
		// That is a valid, if unhelpful, policy.
		return nil, nil
	}

	// A namespaceSelector with no podSelector means "every pod in those
	// namespaces". When each namespace has its own subnet, one rule per
	// namespace expresses that exactly, instead of one rule per pod. On a
	// large namespace that is the difference between one rule and a thousand.
	if p.PodSelector == nil && !needPods {
		out, err := namespaceCIDRPeers(ctx, namespaces, r)
		if err != nil {
			return nil, err
		}
		if out != nil {
			return out, nil
		}
	}

	pods, err := r.Pods(ctx, p.PodSelector, namespaces)
	if err != nil {
		return nil, err
	}

	out := make([]peer, 0, len(pods))
	for i := range pods {
		cidr, err := HostCIDR(pods[i].IP)
		if err != nil {
			return nil, err
		}
		out = append(out, peer{CIDR: cidr, Pod: &pods[i]})
	}
	return out, nil
}

// namespaceCIDRPeers returns one peer per namespace subnet, or nil when
// namespaces do not have their own subnets.
func namespaceCIDRPeers(ctx context.Context, namespaces []string, r Resolver) ([]peer, error) {
	out := make([]peer, 0, len(namespaces))
	for _, ns := range namespaces {
		cidr, err := r.NamespaceCIDR(ctx, ns)
		if err != nil {
			return nil, err
		}
		if cidr == "" {
			// Not using per-namespace subnets, so fall back to pod IPs.
			return nil, nil
		}
		out = append(out, peer{CIDR: cidr, Namespace: ns})
	}
	return out, nil
}

// ingressPorts turns an ingress rule's port list into protocol and range
// pairs.
//
// An empty list means every port, which a security group expresses by leaving
// the range and protocol unset. A named port here belongs to the pods the
// policy selects, so it resolves the same way for every peer.
func ingressPorts(ctx context.Context, ports []netv1.NetworkPolicyPort,
	np *netv1.NetworkPolicy, r Resolver) ([]portRule, error) {

	return resolvePorts(ports, func(i int, name string) ([]int32, error) {
		numbers, err := r.NamedPorts(ctx, &np.Spec.PodSelector, np.Namespace, name)
		if err != nil {
			return nil, fmt.Errorf("port %d: resolving named port %q on the selected pods: %w",
				i, name, err)
		}
		return numbers, nil
	})
}

// egressPorts turns an egress rule's port list into protocol and range pairs
// for one destination.
//
// A named port in an egress rule names a port on the destination pod, so it
// is read from that pod rather than from the pods the policy selects. When
// the destination is a bare CIDR there is no pod to read it from and the
// named port contributes nothing: inventing a number would open a port the
// policy never named.
func egressPorts(ctx context.Context, ports []netv1.NetworkPolicyPort,
	pr peer, np *netv1.NetworkPolicy) ([]portRule, error) {

	return resolvePorts(ports, func(_ int, name string) ([]int32, error) {
		if pr.Pod == nil {
			log.FromContext(ctx).Info(
				"skipping a named port in an egress rule: the destination is a CIDR, "+
					"which has no container ports to resolve the name against",
				"networkPolicy", np.Namespace+"/"+np.Name, "port", name, "destination", pr.CIDR)
			return nil, nil
		}
		return pr.Pod.ContainerPorts[name], nil
	})
}

// resolvePorts is the shared shape of both directions: everything except how
// a named port becomes numbers, which is what named answers.
func resolvePorts(ports []netv1.NetworkPolicyPort,
	named func(index int, name string) ([]int32, error)) ([]portRule, error) {

	if len(ports) == 0 {
		return []portRule{{}}, nil
	}

	var out []portRule
	for i, p := range ports {
		protocol := protoTCP
		if p.Protocol != nil {
			protocol = protocolName(*p.Protocol)
		}

		// No port at all: the protocol is constrained but the ports are not.
		if p.Port == nil {
			out = append(out, portRule{Protocol: protocol})
			continue
		}

		if p.Port.Type == intstr.String {
			numbers, err := named(i, p.Port.StrVal)
			if err != nil {
				return nil, err
			}
			// No pod declares this name, so the rule cannot be expressed.
			// Inventing one would open a port the policy never named.
			for _, n := range numbers {
				out = append(out, portRule{Protocol: protocol, Min: n, Max: n})
			}
			continue
		}

		low := int32(p.Port.IntValue())
		high := low
		if p.EndPort != nil {
			high = *p.EndPort
		}
		if high < low {
			return nil, fmt.Errorf("port %d: endPort %d is below port %d", i, high, low)
		}
		out = append(out, portRule{Protocol: protocol, Min: low, Max: high})
	}

	return out, nil
}

// rulesFor produces one security group rule per port constraint on one peer.
//
// Both a peer and a port constrain the same rule, so every combination is one
// rule. This is where the rule count comes from, and why a policy selecting a
// large namespace without per-namespace subnets produces a great many rules.
func rulesFor(direction kuryrv1.SGDirection, pr peer, ports []portRule,
	np *netv1.NetworkPolicy) ([]kuryrv1.SGRuleSpec, error) {

	etherType, err := EtherTypeOf(pr.CIDR)
	if err != nil {
		return nil, err
	}

	// A rule matching everything needs no remote prefix; leaving it off keeps
	// the rule readable in a Neutron listing.
	remote := pr.CIDR
	if remote == allIPv4 || remote == allIPv6 {
		remote = ""
	}
	description := fmt.Sprintf("kuryr: NetworkPolicy %s/%s", np.Namespace, np.Name)

	out := make([]kuryrv1.SGRuleSpec, 0, len(ports))
	for _, port := range ports {
		rule := kuryrv1.SGRuleSpec{
			Rule: kuryrv1.SGRule{
				Direction:      direction,
				EtherType:      kuryrv1.SGEtherType(etherType),
				Protocol:       port.Protocol,
				PortRangeMin:   port.Min,
				PortRangeMax:   port.Max,
				RemoteIPPrefix: remote,
				Description:    description,
			},
			Namespace: pr.Namespace,
		}
		if pr.Pod != nil {
			rule.AffectedPods = []kuryrv1.AffectedPod{pr.Pod.affected()}
		}
		out = append(out, rule)
	}

	return out, nil
}

// protocolName converts a Kubernetes protocol to the lowercase form Neutron
// expects.
func protocolName(p corev1.Protocol) string {
	switch p {
	case corev1.ProtocolTCP:
		return protoTCP
	case corev1.ProtocolUDP:
		return "udp"
	case corev1.ProtocolSCTP:
		return "sctp"
	default:
		return protoTCP
	}
}

// SortRules puts rules in a stable order.
//
// The translation walks maps and label selectors, so two runs over unchanged
// input can produce the same rules in a different order. Sorting makes the
// CRD byte-identical between runs, which stops the controller from rewriting
// it and waking itself up again for no reason.
func SortRules(rules []kuryrv1.SGRuleSpec) {
	slices.SortStableFunc(rules, func(x, y kuryrv1.SGRuleSpec) int {
		a, b := x.Rule, y.Rule
		return cmp.Or(
			cmp.Compare(a.Direction, b.Direction),
			cmp.Compare(a.EtherType, b.EtherType),
			cmp.Compare(a.Protocol, b.Protocol),
			cmp.Compare(a.RemoteIPPrefix, b.RemoteIPPrefix),
			cmp.Compare(a.PortRangeMin, b.PortRangeMin),
			cmp.Compare(a.PortRangeMax, b.PortRangeMax),
		)
	})
}
