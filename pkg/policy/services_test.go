package policy

import (
	"context"
	"testing"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

func rulesTo(rules []kuryrv1.SGRuleSpec, prefix string) []kuryrv1.SGRuleSpec {
	var out []kuryrv1.SGRuleSpec
	for _, r := range rules {
		if r.Rule.RemoteIPPrefix == prefix {
			out = append(out, r)
		}
	}
	return out
}

// Traffic to a Service leaves the pod addressed to its cluster IP, so an
// egress rule allowing some pods must also allow the Services fronting them.
func TestEgressToAPodAlsoAllowsItsService(t *testing.T) {
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeEgress}
		np.Spec.Egress = []netv1.NetworkPolicyEgressRule{{
			To: []netv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			}},
			Ports: []netv1.NetworkPolicyPort{tcpPort(5432)},
		}}
	})
	ns := np.Namespace
	r := &fakeResolver{
		podIPs:    []string{"10.0.1.5"},
		podLabels: map[string]map[string]string{"10.0.1.5": {"app": "db"}},
		services: []ServiceRef{
			{Namespace: ns, Name: "db", ClusterIPs: []string{"10.96.0.10"}, Selector: map[string]string{"app": "db"}},
			{Namespace: ns, Name: "cache", ClusterIPs: []string{"10.96.0.11"}, Selector: map[string]string{"app": "cache"}},
			{Namespace: ns, Name: "manual", ClusterIPs: []string{"10.96.0.12"}},
		},
	}

	spec := translate(t, np, r)

	via := rulesTo(spec.EgressRules, "10.96.0.10/32")
	if len(via) != 1 {
		t.Fatalf("got %d rules to the db Service, want 1: %+v", len(via), spec.EgressRules)
	}
	if via[0].Rule.PortRangeMin != 5432 || via[0].Rule.Protocol != "tcp" || via[0].Rule.Direction != kuryrv1.SGDirectionEgress {
		t.Errorf("Service rule = %+v, want egress tcp/5432", via[0].Rule)
	}
	for _, other := range []string{"10.96.0.11/32", "10.96.0.12/32"} {
		if got := rulesTo(spec.EgressRules, other); len(got) != 0 {
			t.Errorf("opened egress to %s, which does not front an allowed pod", other)
		}
	}
}

func TestEgressToANamespaceAllowsItsServices(t *testing.T) {
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeEgress}
		np.Spec.Egress = []netv1.NetworkPolicyEgressRule{{
			To: []netv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
		}}
	})
	r := &fakeResolver{
		namespaces:     []string{"shared"},
		namespaceCIDRs: map[string]string{"shared": "10.5.0.0/24"},
		services: []ServiceRef{
			{Namespace: "shared", Name: "dns", ClusterIPs: []string{"10.96.0.53"}, Selector: map[string]string{"k8s-app": "dns"}},
		},
	}

	spec := translate(t, np, r)

	if got := rulesTo(spec.EgressRules, "10.96.0.53/32"); len(got) != 1 {
		t.Errorf("got %d rules to the shared namespace's Service, want 1", len(got))
	}
}

func TestServiceIngressFollowsTheOption(t *testing.T) {
	np := policyWith(func(np *netv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []netv1.PolicyType{netv1.PolicyTypeIngress}
	})
	r := &fakeResolver{services: []ServiceRef{
		{Namespace: np.Namespace, Name: "web", ClusterIPs: []string{"10.96.0.20"}},
	}}

	off := translate(t, np, r)
	if len(off.IngressRules) != 0 {
		t.Errorf("got %d ingress rules with ServiceIngress off, want none", len(off.IngressRules))
	}

	on, err := Translate(context.Background(), np, r, Options{ServiceIngress: true})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	got := rulesTo(on.IngressRules, "10.96.0.20/32")
	if len(got) != 1 || got[0].Rule.Protocol != "" || got[0].Rule.PortRangeMin != 0 {
		t.Errorf("hairpin rules = %+v, want one all-ports ingress rule from the cluster IP", got)
	}
}
