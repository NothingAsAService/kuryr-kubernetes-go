package policy

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// fakeResolver answers selector questions from fixed data.
type fakeResolver struct {
	namespaces     []string
	podIPs         []string
	namespaceCIDRs map[string]string
	namedPorts     map[string][]int32
	// podPorts maps a peer pod's IP to its named container ports. Egress
	// named ports are resolved against the destination, so this is what an
	// egress rule reads instead of namedPorts.
	podPorts map[string]map[string][]int32

	namespacesErr error
	podIPsErr     error
	cidrErr       error
	namedPortErr  error
}

func (f *fakeResolver) Namespaces(_ context.Context, sel *metav1.LabelSelector, policyNamespace string) ([]string, error) {
	if f.namespacesErr != nil {
		return nil, f.namespacesErr
	}
	if sel == nil {
		return []string{policyNamespace}, nil
	}
	return f.namespaces, nil
}

func (f *fakeResolver) Pods(_ context.Context, _ *metav1.LabelSelector, namespaces []string) ([]PodRef, error) {
	if f.podIPsErr != nil {
		return nil, f.podIPsErr
	}
	ns := ""
	if len(namespaces) > 0 {
		ns = namespaces[0]
	}
	out := make([]PodRef, 0, len(f.podIPs))
	for _, ip := range f.podIPs {
		out = append(out, PodRef{
			Namespace:      ns,
			Name:           "pod-" + ip,
			IP:             ip,
			ContainerPorts: f.podPorts[ip],
		})
	}
	return out, nil
}

func (f *fakeResolver) NamespaceCIDR(_ context.Context, ns string) (string, error) {
	if f.cidrErr != nil {
		return "", f.cidrErr
	}
	return f.namespaceCIDRs[ns], nil
}

func (f *fakeResolver) NamedPorts(_ context.Context, _ *metav1.LabelSelector, _, name string) ([]int32, error) {
	if f.namedPortErr != nil {
		return nil, f.namedPortErr
	}
	return f.namedPorts[name], nil
}

func policyWith(mods func(*netv1.NetworkPolicy)) *netv1.NetworkPolicy {
	np := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-web", Namespace: "default"},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
		},
	}
	if mods != nil {
		mods(np)
	}
	return np
}

func tcpPort(n int) netv1.NetworkPolicyPort {
	proto := corev1.ProtocolTCP
	port := intstr.FromInt32(int32(n))
	return netv1.NetworkPolicyPort{Protocol: &proto, Port: &port}
}

func translate(t *testing.T, np *netv1.NetworkPolicy, r Resolver) kuryrv1.KuryrNetworkPolicySpec {
	t.Helper()
	spec, err := Translate(context.Background(), np, r)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	return spec
}

func TestDenyAllIngressProducesNoRules(t *testing.T) {
	// A security group denies by default, so "deny all ingress" is expressed
	// by selecting the pods and writing no rules at all.
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
	})

	spec := translate(t, np, &fakeResolver{})

	if len(spec.IngressRules) != 0 {
		t.Errorf("got %d ingress rules, want none", len(spec.IngressRules))
	}
	if len(spec.PolicyTypes) != 1 || spec.PolicyTypes[0] != "Ingress" {
		t.Errorf("policyTypes = %v, want [Ingress]", spec.PolicyTypes)
	}
}

func TestAllowFromAnywhere(t *testing.T) {
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{}}
	})

	spec := translate(t, np, &fakeResolver{})

	// One rule per address family, with no remote prefix so it matches all.
	if len(spec.IngressRules) != 2 {
		t.Fatalf("got %d rules, want 2 (one per address family)", len(spec.IngressRules))
	}
	families := map[kuryrv1.SGEtherType]bool{}
	for _, r := range spec.IngressRules {
		families[r.Rule.EtherType] = true
		if r.Rule.RemoteIPPrefix != "" {
			t.Errorf("remote prefix = %q, want empty for allow-from-anywhere", r.Rule.RemoteIPPrefix)
		}
		if r.Rule.Direction != kuryrv1.SGDirectionIngress {
			t.Errorf("direction = %q, want ingress", r.Rule.Direction)
		}
	}
	if !families[kuryrv1.SGEtherTypeIPv4] || !families[kuryrv1.SGEtherTypeIPv6] {
		t.Errorf("families = %v, want both IPv4 and IPv6", families)
	}
}

func TestPodSelectorPeerBecomesHostRules(t *testing.T) {
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			}},
			Ports: []netv1.NetworkPolicyPort{tcpPort(8080)},
		}}
	})

	r := &fakeResolver{podIPs: []string{"10.0.1.5", "10.0.1.6"}}
	spec := translate(t, np, r)

	if len(spec.IngressRules) != 2 {
		t.Fatalf("got %d rules, want one per pod", len(spec.IngressRules))
	}
	prefixes := map[string]bool{}
	for _, rule := range spec.IngressRules {
		prefixes[rule.Rule.RemoteIPPrefix] = true
		if rule.Rule.PortRangeMin != 8080 || rule.Rule.PortRangeMax != 8080 {
			t.Errorf("port range = %d-%d, want 8080-8080",
				rule.Rule.PortRangeMin, rule.Rule.PortRangeMax)
		}
		if rule.Rule.Protocol != "tcp" {
			t.Errorf("protocol = %q, want tcp", rule.Rule.Protocol)
		}
		// Each rule must record the pod it came from, so a later label
		// change can be traced back to the rules it invalidates.
		if len(rule.AffectedPods) != 1 {
			t.Errorf("affectedPods = %v, want exactly the pod behind the rule", rule.AffectedPods)
		} else if rule.AffectedPods[0].PodIP == "" {
			t.Error("affectedPods entry has no pod IP")
		}
	}
	if !prefixes["10.0.1.5/32"] || !prefixes["10.0.1.6/32"] {
		t.Errorf("prefixes = %v, want /32 rules for both pods", prefixes)
	}
}

func TestNamespaceSelectorUsesNamespaceCIDR(t *testing.T) {
	// With per-namespace subnets, a whole namespace collapses into one rule
	// instead of one per pod. That is the difference between a workable
	// policy and thousands of security group rules.
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"team": "payments"},
				},
			}},
		}}
	})

	r := &fakeResolver{
		namespaces:     []string{"payments"},
		namespaceCIDRs: map[string]string{"payments": "10.4.0.0/24"},
		podIPs:         []string{"10.4.0.1", "10.4.0.2", "10.4.0.3"},
	}
	spec := translate(t, np, r)

	if len(spec.IngressRules) != 1 {
		t.Fatalf("got %d rules, want 1 covering the namespace subnet", len(spec.IngressRules))
	}
	if got := spec.IngressRules[0].Rule.RemoteIPPrefix; got != "10.4.0.0/24" {
		t.Errorf("remote prefix = %q, want the namespace subnet 10.4.0.0/24", got)
	}
	if got := spec.IngressRules[0].Namespace; got != "payments" {
		t.Errorf("namespace = %q, want payments", got)
	}
}

func TestNamespaceSelectorFallsBackToPodIPs(t *testing.T) {
	// Without per-namespace subnets there is no CIDR to use, so every pod
	// needs its own rule.
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{},
			}},
		}}
	})

	r := &fakeResolver{
		namespaces: []string{"payments"},
		podIPs:     []string{"10.4.0.1", "10.4.0.2"},
	}
	spec := translate(t, np, r)

	if len(spec.IngressRules) != 2 {
		t.Fatalf("got %d rules, want one per pod when there is no namespace subnet",
			len(spec.IngressRules))
	}
}

func TestIPBlockWithExcept(t *testing.T) {
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{
				IPBlock: &netv1.IPBlock{
					CIDR:   "10.0.0.0/8",
					Except: []string{"10.128.0.0/9"},
				},
			}},
		}}
	})

	spec := translate(t, np, &fakeResolver{})

	if len(spec.IngressRules) != 1 {
		t.Fatalf("got %d rules, want 1", len(spec.IngressRules))
	}
	// The excepted half must not appear anywhere in the output.
	if got := spec.IngressRules[0].Rule.RemoteIPPrefix; got != "10.0.0.0/9" {
		t.Errorf("remote prefix = %q, want 10.0.0.0/9 with the excepted half removed", got)
	}
}

func TestPortRange(t *testing.T) {
	proto := corev1.ProtocolTCP
	port := intstr.FromInt32(3000)
	end := int32(3010)

	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			Ports: []netv1.NetworkPolicyPort{{Protocol: &proto, Port: &port, EndPort: &end}},
		}}
	})

	spec := translate(t, np, &fakeResolver{})

	for _, rule := range spec.IngressRules {
		if rule.Rule.PortRangeMin != 3000 || rule.Rule.PortRangeMax != 3010 {
			t.Errorf("port range = %d-%d, want 3000-3010",
				rule.Rule.PortRangeMin, rule.Rule.PortRangeMax)
		}
	}
}

func TestNamedPort(t *testing.T) {
	proto := corev1.ProtocolTCP
	port := intstr.FromString("http")

	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			Ports: []netv1.NetworkPolicyPort{{Protocol: &proto, Port: &port}},
		}}
	})

	r := &fakeResolver{namedPorts: map[string][]int32{"http": {8080}}}
	spec := translate(t, np, r)

	if len(spec.IngressRules) == 0 {
		t.Fatal("expected rules for the named port")
	}
	for _, rule := range spec.IngressRules {
		if rule.Rule.PortRangeMin != 8080 {
			t.Errorf("port = %d, want the resolved named port 8080", rule.Rule.PortRangeMin)
		}
	}
}

func TestUnresolvableNamedPortProducesNoRule(t *testing.T) {
	// If no selected pod declares the name, there is no number to allow.
	// Emitting a rule anyway would open a port the policy never named.
	proto := corev1.ProtocolTCP
	port := intstr.FromString("nonexistent")

	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			Ports: []netv1.NetworkPolicyPort{{Protocol: &proto, Port: &port}},
		}}
	})

	spec := translate(t, np, &fakeResolver{namedPorts: map[string][]int32{}})

	if len(spec.IngressRules) != 0 {
		t.Errorf("got %d rules, want none for an unresolvable named port", len(spec.IngressRules))
	}
}

func TestEgressRules(t *testing.T) {
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeEgress}
		np.Spec.Egress = []netv1.NetworkPolicyEgressRule{{
			To: []netv1.NetworkPolicyPeer{{
				IPBlock: &netv1.IPBlock{CIDR: "192.168.0.0/16"},
			}},
			Ports: []netv1.NetworkPolicyPort{tcpPort(443)},
		}}
	})

	spec := translate(t, np, &fakeResolver{})

	if len(spec.EgressRules) != 1 {
		t.Fatalf("got %d egress rules, want 1", len(spec.EgressRules))
	}
	rule := spec.EgressRules[0].Rule
	if rule.Direction != kuryrv1.SGDirectionEgress {
		t.Errorf("direction = %q, want egress", rule.Direction)
	}
	if rule.RemoteIPPrefix != "192.168.0.0/16" {
		t.Errorf("remote prefix = %q, want 192.168.0.0/16", rule.RemoteIPPrefix)
	}
	// An egress-only policy says nothing about ingress, and it must not say
	// anything about ingress here either: opening ingress in this policy's
	// own group would open it for every pod carrying the group, including
	// pods that a second policy isolates. Whether ingress ends up open is
	// decided per pod by the security groups driver, from policyTypes. See
	// TestPolicySGDoesNotWidenARestrictedDirection in pkg/drivers.
	if len(spec.IngressRules) != 0 {
		t.Fatalf("got %d ingress rules, want none from an egress-only policy",
			len(spec.IngressRules))
	}
	if !slices.Contains(spec.PolicyTypes, string(netv1.PolicyTypeEgress)) ||
		slices.Contains(spec.PolicyTypes, string(netv1.PolicyTypeIngress)) {
		t.Errorf("policyTypes = %v, want egress only: that is what tells the driver "+
			"ingress is unrestricted", spec.PolicyTypes)
	}
}

func TestPolicyTypesAreInferred(t *testing.T) {
	// Kubernetes infers policyTypes when it is omitted: Ingress always,
	// Egress only when egress rules are present.
	withEgress := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.Egress = []netv1.NetworkPolicyEgressRule{{}}
	})
	spec := translate(t, withEgress, &fakeResolver{})
	if len(spec.PolicyTypes) != 2 {
		t.Errorf("policyTypes = %v, want both Ingress and Egress", spec.PolicyTypes)
	}

	ingressOnly := policyWith(nil)
	spec = translate(t, ingressOnly, &fakeResolver{})
	if len(spec.PolicyTypes) != 1 || spec.PolicyTypes[0] != "Ingress" {
		t.Errorf("policyTypes = %v, want [Ingress]", spec.PolicyTypes)
	}
}

func TestProtocolsAreLowercased(t *testing.T) {
	for _, tc := range []struct {
		in   corev1.Protocol
		want string
	}{
		{corev1.ProtocolTCP, "tcp"},
		{corev1.ProtocolUDP, "udp"},
		{corev1.ProtocolSCTP, "sctp"},
	} {
		proto := tc.in
		port := intstr.FromInt32(53)
		np := policyWith(func(np *netv1.NetworkPolicy) {
			np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
			np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
				Ports: []netv1.NetworkPolicyPort{{Protocol: &proto, Port: &port}},
			}}
		})

		spec := translate(t, np, &fakeResolver{})
		if len(spec.IngressRules) == 0 {
			t.Fatalf("protocol %s: no rules produced", tc.in)
		}
		if got := spec.IngressRules[0].Rule.Protocol; got != tc.want {
			t.Errorf("protocol %s translated to %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolverErrorsPropagate(t *testing.T) {
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
		}}
	})

	boom := errors.New("api server unavailable")
	_, err := Translate(context.Background(), np, &fakeResolver{podIPsErr: boom})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the resolver failure", err)
	}
}

func TestBadIPBlockIsReported(t *testing.T) {
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: "not-a-cidr"}}},
		}}
	})

	if _, err := Translate(context.Background(), np, &fakeResolver{}); err == nil {
		t.Fatal("expected an error for an unparseable ipBlock")
	}
}

func TestEndPortBelowPortIsRejected(t *testing.T) {
	proto := corev1.ProtocolTCP
	port := intstr.FromInt32(9000)
	end := int32(80)

	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			Ports: []netv1.NetworkPolicyPort{{Protocol: &proto, Port: &port, EndPort: &end}},
		}}
	})

	if _, err := Translate(context.Background(), np, &fakeResolver{}); err == nil {
		t.Fatal("expected an error when endPort is below port")
	}
}

func TestSortRulesIsStable(t *testing.T) {
	// The CRD must be byte-identical between runs over unchanged input, or
	// the controller rewrites it and wakes itself up forever.
	build := func() []kuryrv1.SGRuleSpec {
		return []kuryrv1.SGRuleSpec{
			{Rule: kuryrv1.SGRule{Direction: "ingress", EtherType: "IPv6", RemoteIPPrefix: "fd00::/64"}},
			{Rule: kuryrv1.SGRule{Direction: "egress", EtherType: "IPv4", RemoteIPPrefix: "10.0.0.0/8"}},
			{Rule: kuryrv1.SGRule{Direction: "ingress", EtherType: "IPv4", RemoteIPPrefix: "10.0.0.0/8", PortRangeMin: 80}},
			{Rule: kuryrv1.SGRule{Direction: "ingress", EtherType: "IPv4", RemoteIPPrefix: "10.0.0.0/8", PortRangeMin: 22}},
		}
	}

	first := build()
	SortRules(first)
	for i := 0; i < 5; i++ {
		again := build()
		SortRules(again)
		for j := range first {
			if first[j].Rule != again[j].Rule {
				t.Fatalf("sort is not deterministic at index %d", j)
			}
		}
	}

	// Egress sorts before ingress, and within ingress the lower port first.
	if first[0].Rule.Direction != kuryrv1.SGDirectionEgress {
		t.Errorf("first rule direction = %q, want egress", first[0].Rule.Direction)
	}
	if first[1].Rule.PortRangeMin != 22 {
		t.Errorf("second rule port = %d, want 22", first[1].Rule.PortRangeMin)
	}
}

func TestIngressOnlyPolicyRecordsThatEgressIsUngoverned(t *testing.T) {
	// A policy that only governs ingress must not block egress -- getting
	// that wrong silently breaks DNS for every pod the policy selects, which
	// is the classic way to break a cluster.
	//
	// What is checked here is only that the policy declares it governs
	// ingress alone and contributes no egress rules. The pod is actually let
	// out by the shared allow-all group the security groups driver adds when
	// no policy on that pod governs egress; doing it there rather than here
	// is what stops this policy from also letting out a pod that a second
	// policy deliberately confines.
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
		np.Spec.Ingress = []netv1.NetworkPolicyIngressRule{{
			From: []netv1.NetworkPolicyPeer{{
				IPBlock: &netv1.IPBlock{CIDR: "10.0.0.0/8"},
			}},
		}}
	})

	spec := translate(t, np, &fakeResolver{})

	if len(spec.EgressRules) != 0 {
		t.Fatalf("got %d egress rules, want none from an ingress-only policy", len(spec.EgressRules))
	}
	if len(spec.IngressRules) == 0 {
		t.Fatal("got no ingress rules from a policy that does govern ingress")
	}
	if !slices.Contains(spec.PolicyTypes, string(netv1.PolicyTypeIngress)) ||
		slices.Contains(spec.PolicyTypes, string(netv1.PolicyTypeEgress)) {
		t.Errorf("policyTypes = %v, want ingress only: that is what tells the driver "+
			"egress is unrestricted", spec.PolicyTypes)
	}
}

// The three tests below cover a named port in an egress rule.
//
// Kubernetes says a named port in an egress rule names a port on the
// *destination* pod. Upstream kuryr-kubernetes resolves ingress named ports against the
// policy's own pods, which is right, and this package used to do the same for
// egress, which is not: two backends can map the same name to different
// numbers, so the rule ends up opening the wrong port or none at all.

func TestEgressNamedPortResolvesOnTheDestinationPod(t *testing.T) {
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeEgress}
		np.Spec.Egress = []netv1.NetworkPolicyEgressRule{{
			To: []netv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			}},
			Ports: []netv1.NetworkPolicyPort{namedPort()},
		}}
	})

	r := &fakeResolver{
		podIPs: []string{"10.0.1.5", "10.0.1.6"},
		// The two destinations disagree about what "metrics" means, which is
		// exactly what resolving against the policy's own pods cannot express.
		podPorts: map[string]map[string][]int32{
			"10.0.1.5": {"metrics": {9100}},
			"10.0.1.6": {"metrics": {9200}},
		},
		// Deliberately wrong, so a regression that reads this instead is
		// visible rather than accidentally right.
		namedPorts: map[string][]int32{"metrics": {1234}},
	}
	spec := translate(t, np, r)

	got := map[string]int32{}
	for _, rule := range spec.EgressRules {
		got[rule.Rule.RemoteIPPrefix] = rule.Rule.PortRangeMin
	}
	want := map[string]int32{"10.0.1.5/32": 9100, "10.0.1.6/32": 9200}
	if len(got) != len(want) {
		t.Fatalf("rules = %v, want %v", got, want)
	}
	for cidr, port := range want {
		if got[cidr] != port {
			t.Errorf("rule for %s opens port %d, want %d", cidr, got[cidr], port)
		}
	}
}

func TestEgressNamedPortSkipsDestinationsThatDoNotDeclareIt(t *testing.T) {
	// A destination with no such port must produce no rule at all. Falling
	// back to some other pod's number would open a port on a pod the policy
	// never named that port for.
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeEgress}
		np.Spec.Egress = []netv1.NetworkPolicyEgressRule{{
			To: []netv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			}},
			Ports: []netv1.NetworkPolicyPort{namedPort()},
		}}
	})

	r := &fakeResolver{
		podIPs: []string{"10.0.1.5", "10.0.1.6"},
		podPorts: map[string]map[string][]int32{
			"10.0.1.5": {"metrics": {9100}},
		},
	}
	spec := translate(t, np, r)

	if len(spec.EgressRules) != 1 {
		t.Fatalf("got %d egress rules, want only the one for the pod that declares the port",
			len(spec.EgressRules))
	}
	if got := spec.EgressRules[0].Rule.RemoteIPPrefix; got != "10.0.1.5/32" {
		t.Errorf("rule is for %q, want 10.0.1.5/32", got)
	}
}

func TestEgressNamedPortForcesPerPodRulesOverNamespaceCIDRs(t *testing.T) {
	// A namespaceSelector normally collapses to the namespace's subnet, which
	// is one rule instead of hundreds. It cannot here: the port number is a
	// property of the individual destination, and a subnet rule has no pod to
	// read it from.
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeEgress}
		np.Spec.Egress = []netv1.NetworkPolicyEgressRule{{
			To: []netv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"team": "payments"},
				},
			}},
			Ports: []netv1.NetworkPolicyPort{namedPort()},
		}}
	})

	r := &fakeResolver{
		namespaces:     []string{"payments"},
		namespaceCIDRs: map[string]string{"payments": "10.4.0.0/24"},
		podIPs:         []string{"10.4.0.1"},
		podPorts: map[string]map[string][]int32{
			"10.4.0.1": {"metrics": {9100}},
		},
	}
	spec := translate(t, np, r)

	if len(spec.EgressRules) != 1 {
		t.Fatalf("got %d egress rules, want 1", len(spec.EgressRules))
	}
	rule := spec.EgressRules[0].Rule
	if rule.RemoteIPPrefix != "10.4.0.1/32" {
		t.Errorf("remote prefix = %q, want the pod address rather than the namespace subnet",
			rule.RemoteIPPrefix)
	}
	if rule.PortRangeMin != 9100 || rule.PortRangeMax != 9100 {
		t.Errorf("port range = %d-%d, want 9100-9100", rule.PortRangeMin, rule.PortRangeMax)
	}
}

func TestEgressNamedPortToACIDRProducesNoRule(t *testing.T) {
	// An ipBlock destination has no container ports. Upstream kuryr-kubernetes logs this
	// and moves on, and there is nothing better available: guessing a number
	// would open a port the policy never named.
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeEgress}
		np.Spec.Egress = []netv1.NetworkPolicyEgressRule{{
			To:    []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: "192.168.0.0/16"}}},
			Ports: []netv1.NetworkPolicyPort{namedPort()},
		}}
	})

	spec := translate(t, np, &fakeResolver{})

	if len(spec.EgressRules) != 0 {
		t.Fatalf("got %d egress rules, want none: a CIDR has no named ports", len(spec.EgressRules))
	}
}

// namedPort is the "metrics" named port every named-port case in this file
// uses; the tests differ in what the policy does with it, not in its name.
func namedPort() netv1.NetworkPolicyPort {
	proto := corev1.ProtocolTCP
	port := intstr.FromString("metrics")
	return netv1.NetworkPolicyPort{Protocol: &proto, Port: &port}
}
