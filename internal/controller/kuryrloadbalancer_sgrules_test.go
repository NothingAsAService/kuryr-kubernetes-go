package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

// vipSGID is the security group Octavia creates on a VIP port. Every test in
// this file uses the same one; what varies is the rules inside it.
const vipSGID = "vip-sg"

// vipRules returns the rules the fake cloud holds for the VIP security group.
func vipRules(cloud *fakeLBCloud) []*openstack.SGRule {
	rules, err := cloud.ListSGRules(context.Background(), vipSGID)
	if err != nil {
		panic(err)
	}
	return rules
}

// enforcing builds a reconciler with the Amphora-style VIP rule enforcement
// switched on, and a VIP port that already carries the group Octavia creates.
func enforcing(t *testing.T, cloud *fakeLBCloud, klb *kuryrv1.KuryrLoadBalancer) *KuryrLoadBalancerReconciler {
	t.Helper()
	r := newKLBReconciler(t, cloud, nil, klb)
	r.Config.OctaviaDefaults.EnforceSGRules = true
	r.Config.NeutronDefaults.PodSecurityGroups = []string{"default-sg"}
	return r
}

// attachVIPGroup gives the load balancer's VIP port the group Octavia would
// have created for it.
func attachVIPGroup(t *testing.T, cloud *fakeLBCloud, r *KuryrLoadBalancerReconciler,
	klb *kuryrv1.KuryrLoadBalancer) {

	t.Helper()
	portID := readKLBObject(t, r, klb).Status.LoadBalancer.PortID
	cloud.mu.Lock()
	cloud.ports[portID].SecurityGroups = []string{vipSGID}
	cloud.mu.Unlock()
}

func TestVIPRulesOpenTheListenerWhenNoPolicyApplies(t *testing.T) {
	// The Service is on the default groups, so no NetworkPolicy governs it
	// and the listener has to be reachable from anywhere.
	cloud := newFakeLBCloud()
	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.SecurityGroupIDs = []string{"default-sg"}
	})
	r := enforcing(t, cloud, klb)

	reconcileKLB(t, r, klb)
	attachVIPGroup(t, cloud, r, klb)
	settle(t, r, klb)

	rules := vipRules(cloud)
	if len(rules) != 1 {
		t.Fatalf("got %d VIP rules, want 1", len(rules))
	}
	if rules[0].PortRangeMin != 80 || rules[0].PortRangeMax != 80 {
		t.Errorf("rule covers ports %d-%d, want the listener port 80",
			rules[0].PortRangeMin, rules[0].PortRangeMax)
	}
	if rules[0].RemoteIPPrefix != "" {
		t.Errorf("remote prefix = %q, want empty for an unrestricted Service",
			rules[0].RemoteIPPrefix)
	}
	if rules[0].Direction != "ingress" || rules[0].Protocol != "tcp" {
		t.Errorf("rule = %+v, want an ingress TCP rule", rules[0])
	}
}

func TestVIPRulesMirrorThePolicyOfTheBackends(t *testing.T) {
	// With the Amphora provider the source address is rewritten, so what the
	// pods' policy allows has to be re-expressed on the VIP.
	cloud := newFakeLBCloud()
	if _, err := cloud.CreateSGRule(context.Background(), openstack.SGRule{
		SecurityGroupID: "policy-sg",
		Direction:       "ingress",
		EtherType:       "IPv4",
		Protocol:        "tcp",
		PortRangeMin:    8080,
		PortRangeMax:    8080,
		RemoteIPPrefix:  "10.0.2.0/24",
	}); err != nil {
		t.Fatalf("seeding the policy rule: %v", err)
	}

	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.SecurityGroupIDs = []string{"policy-sg"}
	})
	r := enforcing(t, cloud, klb)

	reconcileKLB(t, r, klb)
	attachVIPGroup(t, cloud, r, klb)
	settle(t, r, klb)

	rules := vipRules(cloud)
	if len(rules) != 1 {
		t.Fatalf("got %d VIP rules, want 1 mirroring the policy", len(rules))
	}
	// The prefix carries over, but the port becomes the *listener* port: that
	// is what a client connects to.
	if rules[0].RemoteIPPrefix != "10.0.2.0/24" {
		t.Errorf("remote prefix = %q, want the policy's 10.0.2.0/24", rules[0].RemoteIPPrefix)
	}
	if rules[0].PortRangeMin != 80 {
		t.Errorf("port = %d, want the listener port 80, not the backend port",
			rules[0].PortRangeMin)
	}
}

func TestVIPRulesIgnorePolicyRulesForOtherPorts(t *testing.T) {
	// A policy that only opens 9090 says nothing about the traffic this
	// listener forwards to 8080, so it must not open the VIP.
	cloud := newFakeLBCloud()
	if _, err := cloud.CreateSGRule(context.Background(), openstack.SGRule{
		SecurityGroupID: "policy-sg",
		Direction:       "ingress",
		EtherType:       "IPv4",
		Protocol:        "tcp",
		PortRangeMin:    9090,
		PortRangeMax:    9090,
		RemoteIPPrefix:  "10.0.2.0/24",
	}); err != nil {
		t.Fatalf("seeding the policy rule: %v", err)
	}

	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.SecurityGroupIDs = []string{"policy-sg"}
	})
	r := enforcing(t, cloud, klb)

	reconcileKLB(t, r, klb)
	attachVIPGroup(t, cloud, r, klb)
	settle(t, r, klb)

	if rules := vipRules(cloud); len(rules) != 0 {
		t.Errorf("got %d VIP rules, want none: the policy does not cover this port", len(rules))
	}
}

func TestVIPRulesAreRemovedWhenNoLongerWanted(t *testing.T) {
	cloud := newFakeLBCloud()
	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.SecurityGroupIDs = []string{"default-sg"}
	})
	r := enforcing(t, cloud, klb)

	reconcileKLB(t, r, klb)
	attachVIPGroup(t, cloud, r, klb)
	settle(t, r, klb)

	if len(vipRules(cloud)) != 1 {
		t.Fatalf("expected one VIP rule before the change")
	}

	// The Service moves to a different port.
	current := readKLBObject(t, r, klb)
	current.Spec.Ports = []kuryrv1.ServicePort{{
		Name: "http", Protocol: corev1.ProtocolTCP, Port: 8081, TargetPort: "web-port",
	}}
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatalf("updating the spec: %v", err)
	}
	settle(t, r, klb)

	rules := vipRules(cloud)
	if len(rules) != 1 {
		t.Fatalf("got %d VIP rules, want 1 for the new listener", len(rules))
	}
	if rules[0].PortRangeMin != 8081 {
		t.Errorf("VIP rule covers port %d, want the new listener port 8081", rules[0].PortRangeMin)
	}
}

func TestVIPRulesLeaveForeignRulesAlone(t *testing.T) {
	// Octavia puts its own rules on the VIP group, and an operator may too.
	// Deleting those would break the load balancer.
	cloud := newFakeLBCloud()
	foreign, err := cloud.CreateSGRule(context.Background(), openstack.SGRule{
		SecurityGroupID: "vip-sg",
		Direction:       "ingress",
		EtherType:       "IPv4",
		Protocol:        "tcp",
		PortRangeMin:    22,
		PortRangeMax:    22,
		Description:     "octavia amphora management",
	})
	if err != nil {
		t.Fatalf("seeding the foreign rule: %v", err)
	}

	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.SecurityGroupIDs = []string{"default-sg"}
	})
	r := enforcing(t, cloud, klb)

	reconcileKLB(t, r, klb)
	attachVIPGroup(t, cloud, r, klb)
	settle(t, r, klb)

	found := false
	for _, rule := range vipRules(cloud) {
		if rule.ID == foreign.ID {
			found = true
		}
	}
	if !found {
		t.Error("a rule this controller did not create was deleted")
	}
}

func TestVIPRulesDoNothingWithoutAVIPGroup(t *testing.T) {
	// The OVN provider gives the VIP port no security group at all.
	cloud := newFakeLBCloud()
	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.SecurityGroupIDs = []string{"default-sg"}
	})
	r := enforcing(t, cloud, klb)

	settle(t, r, klb)

	cloud.mu.Lock()
	total := len(cloud.sgRules)
	cloud.mu.Unlock()
	if total != 0 {
		t.Errorf("created %d rules with no VIP security group to put them on", total)
	}
}
