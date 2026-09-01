package controller

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/policy"
)

// vipRuleMarker tags the rules this controller manages on the VIP security
// group, so rules Octavia or an operator put there are left alone.
const vipRuleMarker = "kuryr: load balancer listener"

// syncVIPSecurityRules opens the listener ports on the load balancer's own
// security group.
//
// Why this exists: with the Amphora provider the load balancer rewrites the
// source address, so a NetworkPolicy applied to the backend pods sees traffic
// coming from the amphora, not from the original client. The policy is
// therefore re-expressed on the VIP: whatever a pod's policy allows in on a
// given protocol is allowed in on the matching listener port.
//
// It does nothing with the OVN provider, which has no VIP security group, and
// nothing when enforce_sg_rules is off.
func (r *KuryrLoadBalancerReconciler) syncVIPSecurityRules(ctx context.Context,
	klb *kuryrv1.KuryrLoadBalancer, lb *openstack.LoadBalancer) error {

	if !r.Config.OctaviaDefaults.EnforceSGRules || lb.VIPPortID == "" {
		return nil
	}

	vipSG, err := r.vipSecurityGroup(ctx, lb.VIPPortID)
	if err != nil {
		return err
	}
	if vipSG == "" {
		// The OVN provider gives the VIP port no security group. There is
		// nothing to enforce and nothing to fix.
		return nil
	}

	desired, err := r.desiredVIPRules(ctx, klb, vipSG)
	if err != nil {
		return err
	}

	existing, err := r.OpenStack.ListSGRules(ctx, vipSG)
	if err != nil {
		return fmt.Errorf("listing rules of the VIP security group %s: %w", vipSG, err)
	}

	have := map[string]*openstack.SGRule{}
	for _, rule := range existing {
		if !strings.HasPrefix(rule.Description, vipRuleMarker) {
			// Not ours; Octavia adds its own and an operator may too.
			continue
		}
		have[vipRuleKey(*rule)] = rule
	}

	created := 0
	for _, want := range desired {
		key := vipRuleKey(want)
		if _, ok := have[key]; ok {
			delete(have, key)
			continue
		}
		if _, err := r.OpenStack.CreateSGRule(ctx, want); err != nil {
			return fmt.Errorf("opening listener port %d on the VIP security group %s: %w",
				want.PortRangeMin, vipSG, err)
		}
		created++
	}

	// Whatever is left in have is a rule we created for something that is no
	// longer wanted.
	removed := 0
	for _, stale := range have {
		if err := r.OpenStack.DeleteSGRule(ctx, stale.ID); err != nil {
			if openstack.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("removing a stale VIP security group rule %s: %w", stale.ID, err)
		}
		removed++
	}

	if created > 0 || removed > 0 {
		logVIPRules(ctx, vipSG, created, removed)
	}
	return nil
}

// vipSecurityGroup returns the security group Octavia put on the VIP port.
func (r *KuryrLoadBalancerReconciler) vipSecurityGroup(ctx context.Context, portID string) (string, error) {
	port, err := r.OpenStack.GetPort(ctx, portID)
	if err != nil {
		if openstack.IsNotFound(err) {
			// The port went away under us; the next pass re-reads the load
			// balancer and notices.
			return "", nil
		}
		return "", fmt.Errorf("reading the load balancer VIP port %s: %w", portID, err)
	}
	if len(port.SecurityGroups) == 0 {
		return "", nil
	}
	return port.SecurityGroups[0], nil
}

// desiredVIPRules works out which ingress rules the VIP should carry.
//
// For each listener, the rules of the security groups applied to the Service
// are examined: an ingress rule on the same protocol whose port range covers
// the backend port becomes a rule on the VIP for the *listener* port, keeping
// the remote prefix. When the Service carries only the default groups no
// policy applies to it, so the listener port is opened to everything, which
// is what an unrestricted Service means.
func (r *KuryrLoadBalancerReconciler) desiredVIPRules(ctx context.Context,
	klb *kuryrv1.KuryrLoadBalancer, vipSG string) ([]openstack.SGRule, error) {

	etherType, err := r.vipEtherType(ctx, klb)
	if err != nil {
		return nil, err
	}

	defaults := map[string]bool{}
	for _, id := range r.Config.NeutronDefaults.PodSecurityGroups {
		defaults[id] = true
	}

	var out []openstack.SGRule
	for _, listener := range klb.Status.Listeners {
		protocol := strings.ToLower(string(listener.Protocol))
		description := fmt.Sprintf("%s %s", vipRuleMarker, listener.Name)

		open := func(remote string) {
			out = append(out, openstack.SGRule{
				SecurityGroupID: vipSG,
				Direction:       "ingress",
				EtherType:       etherType,
				Protocol:        protocol,
				PortRangeMin:    int(listener.Port),
				PortRangeMax:    int(listener.Port),
				RemoteIPPrefix:  remote,
				Description:     description,
			})
		}

		unrestricted := false
		var prefixes []string

		for _, sgID := range klb.Spec.SecurityGroupIDs {
			if sgID == vipSG {
				continue
			}
			if defaults[sgID] {
				// The Service is on the default groups, so no NetworkPolicy
				// governs it and the listener is open.
				unrestricted = true
				break
			}

			rules, err := r.OpenStack.ListSGRules(ctx, sgID)
			if err != nil {
				return nil, fmt.Errorf("listing rules of security group %s to mirror them "+
					"onto the load balancer: %w", sgID, err)
			}
			for _, rule := range rules {
				if rule.Direction != "ingress" || rule.Protocol != protocol {
					continue
				}
				if !coversPort(rule, listener.Port, klb) {
					continue
				}
				if rule.RemoteIPPrefix == "" {
					unrestricted = true
					break
				}
				prefixes = append(prefixes, rule.RemoteIPPrefix)
			}
			if unrestricted {
				break
			}
		}

		if unrestricted {
			open("")
			continue
		}
		seen := map[string]bool{}
		for _, p := range prefixes {
			if seen[p] {
				continue
			}
			seen[p] = true
			open(p)
		}
	}

	return out, nil
}

// coversPort reports whether a pod rule permits the traffic this listener
// forwards.
//
// A rule with no port range covers every port. Otherwise the backend port,
// not the listener port, has to fall inside it: that is the port the pod
// actually receives on.
func coversPort(rule *openstack.SGRule, listenerPort int32, klb *kuryrv1.KuryrLoadBalancer) bool {
	if rule.PortRangeMin == 0 && rule.PortRangeMax == 0 {
		return true
	}

	for _, backendPort := range backendPortsForListener(klb, listenerPort) {
		if int(backendPort) >= rule.PortRangeMin && int(backendPort) <= rule.PortRangeMax {
			return true
		}
	}
	return false
}

// backendPortsForListener returns the ports the backends of a listener serve.
func backendPortsForListener(klb *kuryrv1.KuryrLoadBalancer, listenerPort int32) []int32 {
	var servicePort *kuryrv1.ServicePort
	for i := range klb.Spec.Ports {
		if klb.Spec.Ports[i].Port == listenerPort {
			servicePort = &klb.Spec.Ports[i]
			break
		}
	}
	if servicePort == nil {
		return nil
	}

	var out []int32
	for i := range klb.Spec.Subsets {
		if port, ok := backendPort(&klb.Spec.Subsets[i], *servicePort); ok {
			out = append(out, port)
		}
	}
	if len(out) == 0 {
		// No backends yet. Fall back to the listener port so a rule that
		// would match is not discarded before the endpoints appear.
		out = append(out, listenerPort)
	}
	return out
}

// vipEtherType returns the address family of the service subnet, which is the
// family every VIP rule is written in.
func (r *KuryrLoadBalancerReconciler) vipEtherType(ctx context.Context,
	klb *kuryrv1.KuryrLoadBalancer) (string, error) {

	subnet, err := r.OpenStack.GetSubnet(ctx, klb.Spec.SubnetID)
	if err != nil {
		return "", fmt.Errorf("reading the service subnet %s: %w", klb.Spec.SubnetID, err)
	}
	etherType, err := policy.EtherTypeOf(subnet.CIDR)
	if err != nil {
		return "", fmt.Errorf("service subnet %s: %w", klb.Spec.SubnetID, err)
	}
	return etherType, nil
}

// vipRuleKey identifies a VIP rule by what it allows.
func vipRuleKey(r openstack.SGRule) string {
	return fmt.Sprintf("%s|%s|%s|%d|%d|%s",
		r.Direction, r.EtherType, r.Protocol, r.PortRangeMin, r.PortRangeMax, r.RemoteIPPrefix)
}

// cidrContains reports whether an address is inside a prefix.
func cidrContains(cidr, ip string) (bool, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return false, fmt.Errorf("unparseable subnet CIDR %q: %w", cidr, err)
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false, fmt.Errorf("unparseable address %q: %w", ip, err)
	}
	return prefix.Contains(addr), nil
}

// logVIPRules reports what changed on the VIP security group.
func logVIPRules(ctx context.Context, sgID string, created, removed int) {
	log.FromContext(ctx).Info("VIP security group rules synced",
		"securityGroupID", sgID, "created", created, "removed", removed)
}
