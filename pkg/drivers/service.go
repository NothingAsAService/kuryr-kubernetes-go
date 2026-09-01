package drivers

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

func init() {
	RegisterServiceSubnetDriver("default", newDefaultServiceSubnetDriver)
	RegisterServiceSecurityGroupsDriver("default", newDefaultServiceSGDriver)
	RegisterServiceSecurityGroupsDriver("policy", newPolicyServiceSGDriver)
}

// --- service subnet ----------------------------------------------------

// defaultServiceSubnetDriver puts every load balancer VIP on one subnet named
// in kuryr.conf. That subnet is where ClusterIPs come from, so it is the same
// for the whole cluster by construction.
type defaultServiceSubnetDriver struct {
	os       *openstack.Client
	subnetID string
}

func newDefaultServiceSubnetDriver(d Deps) (SubnetDriver, error) {
	if d.Config.NeutronDefaults.ServiceSubnet == "" {
		return nil, fmt.Errorf(
			"[neutron_defaults]service_subnet is required by the default service subnet driver")
	}
	return &defaultServiceSubnetDriver{
		os:       d.OpenStack,
		subnetID: d.Config.NeutronDefaults.ServiceSubnet,
	}, nil
}

// SubnetsFor returns the configured service subnet, the one the cluster's
// service CIDR is carved from.
func (s *defaultServiceSubnetDriver) SubnetsFor(ctx context.Context, _ client.Object,
	_ string) (vif.SubnetSet, error) {

	sub, err := s.os.GetSubnet(ctx, s.subnetID)
	if err != nil {
		return nil, err
	}
	return vif.SubnetSet{sub.ID: sub}, nil
}

// --- service security groups -------------------------------------------

// defaultServiceSGDriver applies the same fixed groups as pods get.
//
// The load balancer VIP port sits in front of the pods, so a group that lets
// traffic reach a pod has to let it reach the VIP too.
type defaultServiceSGDriver struct{ groups []string }

func newDefaultServiceSGDriver(d Deps) (SecurityGroupsDriver, error) {
	if len(d.Config.NeutronDefaults.PodSecurityGroups) == 0 {
		return nil, fmt.Errorf(
			"[neutron_defaults]pod_security_groups is required by the default service " +
				"security groups driver")
	}
	return &defaultServiceSGDriver{groups: d.Config.NeutronDefaults.PodSecurityGroups}, nil
}

// SecurityGroupsFor returns the fixed set of groups from kuryr.conf. The copy
// keeps a caller from mutating the configured slice underneath us.
func (s *defaultServiceSGDriver) SecurityGroupsFor(context.Context, client.Object,
	string) ([]string, error) {

	return append([]string(nil), s.groups...), nil
}

// policyServiceSGDriver gives a load balancer the same groups as the pods it
// fronts.
//
// A Service is not itself selected by a NetworkPolicy; its backends are. So
// the groups are taken from the pods the Service selects. A Service with no
// selector has no backends to take them from and falls back to the defaults,
// which matches how NetworkPolicy leaves such a Service alone.
type policyServiceSGDriver struct {
	k8s      client.Client
	pods     SecurityGroupsDriver
	defaults []string
}

func newPolicyServiceSGDriver(d Deps) (SecurityGroupsDriver, error) {
	if d.K8s == nil {
		return nil, fmt.Errorf("the policy service security groups driver needs a Kubernetes client")
	}
	if len(d.Config.NeutronDefaults.PodSecurityGroups) == 0 {
		return nil, fmt.Errorf(
			"[neutron_defaults]pod_security_groups is required by the policy service " +
				"security groups driver, used for Services without a selector")
	}
	pods, err := newPolicySGDriver(d)
	if err != nil {
		return nil, err
	}
	return &policyServiceSGDriver{
		k8s:      d.K8s,
		pods:     pods,
		defaults: d.Config.NeutronDefaults.PodSecurityGroups,
	}, nil
}

// SecurityGroupsFor returns the union of the groups on the pods behind the
// Service, so the load balancer is reachable by exactly what the backends
// allow.
func (s *policyServiceSGDriver) SecurityGroupsFor(ctx context.Context, obj client.Object,
	projectID string) ([]string, error) {

	svc, ok := obj.(*corev1.Service)
	if !ok {
		return nil, fmt.Errorf("the policy service security groups driver expects a Service, got %T", obj)
	}

	if len(svc.Spec.Selector) == 0 {
		// No selector means no backends Kuryr knows about, so there is no
		// policy to inherit. NetworkPolicy does not reach such a Service.
		return append([]string(nil), s.defaults...), nil
	}

	var list corev1.PodList
	err := s.k8s.List(ctx, &list,
		client.InNamespace(svc.Namespace),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(svc.Spec.Selector)})
	if err != nil {
		return nil, fmt.Errorf("listing pods behind Service %s/%s: %w",
			svc.Namespace, svc.Name, err)
	}

	// Every backend of a Service is expected to carry the same labels, so
	// every backend gets the same groups. Taking the union rather than the
	// first pod's set is both cheap and correct when that expectation does
	// not hold.
	seen := map[string]bool{}
	var groups []string
	for i := range list.Items {
		pod := &list.Items[i]
		if pod.Spec.HostNetwork {
			// A host-network backend has no Neutron port and no policy group.
			continue
		}
		podGroups, err := s.pods.SecurityGroupsFor(ctx, pod, projectID)
		if err != nil {
			return nil, err
		}
		for _, g := range podGroups {
			if !seen[g] {
				seen[g] = true
				groups = append(groups, g)
			}
		}
	}

	if len(groups) == 0 {
		// The Service has a selector but nothing behind it yet.
		return append([]string(nil), s.defaults...), nil
	}

	slices.Sort(groups)
	return groups, nil
}

// --- public IP ---------------------------------------------------------

// PublicIP is a floating IP acquired for a LoadBalancer Service.
type PublicIP struct {
	ID      string
	Address string
	// FromPool records whether Kuryr allocated the address. An address the
	// user named is never released, because it was not ours to release.
	FromPool bool
}

// PublicIPDriver acquires and releases the externally reachable address of a
// Service of type LoadBalancer.
type PublicIPDriver interface {
	// Acquire returns the address to associate, or nil when the Service does
	// not need one.
	Acquire(ctx context.Context, svc *kuryrv1.KuryrLoadBalancer, projectID, portID string) (*PublicIP, error)
	// Associate points an acquired address at the load balancer VIP port.
	Associate(ctx context.Context, ip *PublicIP, portID string) error
	// Release gives an address back, if Kuryr allocated it.
	Release(ctx context.Context, ip *PublicIP) error
}

// FloatingIPDriver acquires Neutron floating IPs from the external network
// named in kuryr.conf.
type FloatingIPDriver struct {
	os        *openstack.Client
	networkID string
	subnetID  string
}

// NewFloatingIPDriver builds the floating IP driver, or reports that it
// cannot work with the current configuration.
func NewFloatingIPDriver(d Deps) (PublicIPDriver, error) {
	if d.Config.NeutronDefaults.ExternalSvcNet == "" {
		return nil, fmt.Errorf(
			"[neutron_defaults]external_svc_net is required to serve Services of type " +
				"LoadBalancer; set it, or stop using that Service type")
	}
	return &FloatingIPDriver{
		os:        d.OpenStack,
		networkID: d.Config.NeutronDefaults.ExternalSvcNet,
		subnetID:  d.Config.NeutronDefaults.ExternalSvcSubnet,
	}, nil
}

// Acquire returns the floating IP for a LoadBalancer Service.
//
// An address the Service names in spec.loadBalancerIP must already exist as a
// floating IP in the project: Kuryr associates it but never creates it, since
// an arbitrary address may not belong to the configured external network.
// Otherwise one is allocated from the pool.
func (f *FloatingIPDriver) Acquire(ctx context.Context, klb *kuryrv1.KuryrLoadBalancer,
	projectID, portID string) (*PublicIP, error) {

	if klb.Spec.Type != string(corev1.ServiceTypeLoadBalancer) {
		return nil, nil
	}
	if portID == "" {
		return nil, fmt.Errorf("the load balancer for Service %s/%s has no VIP port to associate",
			klb.Namespace, klb.Name)
	}

	if requested := klb.Spec.LoadBalancerIP; requested != "" {
		fip, err := f.os.FindFloatingIPByAddress(ctx, requested)
		if err != nil {
			return nil, err
		}
		if fip == nil {
			return nil, fmt.Errorf("load balancer IP %s requested by Service %s/%s is not an existing "+
				"floating IP in this project", requested, klb.Namespace, klb.Name)
		}
		if fip.PortID != "" && fip.PortID != portID {
			return nil, fmt.Errorf("floating IP %s is already associated with port %s",
				requested, fip.PortID)
		}
		return &PublicIP{ID: fip.ID, Address: fip.Address, FromPool: false}, nil
	}

	fip, err := f.os.AllocateFloatingIP(ctx, openstack.FloatingIPSpec{
		NetworkID:   f.networkID,
		SubnetID:    f.subnetID,
		ProjectID:   projectID,
		PortID:      portID,
		Description: fmt.Sprintf("kuryr: Service %s/%s", klb.Namespace, klb.Name),
	})
	if err != nil {
		return nil, err
	}
	return &PublicIP{ID: fip.ID, Address: fip.Address, FromPool: true}, nil
}

// Release gives a pooled address back. An address the user named is left
// alone: releasing it would delete something Kuryr did not create.
func (f *FloatingIPDriver) Release(ctx context.Context, ip *PublicIP) error {
	if ip == nil || ip.ID == "" {
		return nil
	}
	if !ip.FromPool {
		return f.os.DisassociateFloatingIP(ctx, ip.ID)
	}
	return f.os.DeleteFloatingIP(ctx, ip.ID)
}

// Associate points an acquired address at the load balancer VIP port.
//
// Allocation from the pool already associates, so this only has work to do
// for an address the user named.
func (f *FloatingIPDriver) Associate(ctx context.Context, ip *PublicIP, portID string) error {
	if ip == nil || ip.ID == "" || portID == "" {
		return nil
	}
	return f.os.AssociateFloatingIP(ctx, ip.ID, portID)
}
