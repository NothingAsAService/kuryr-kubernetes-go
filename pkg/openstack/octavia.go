package openstack

import (
	"context"
	"errors"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/providers"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// Octavia provisioning states. A load balancer only accepts changes while it
// is ACTIVE; anything else means an operation is in flight or has failed.
const (
	ProvisioningActive        = "ACTIVE"
	ProvisioningError         = "ERROR"
	ProvisioningPendingCreate = "PENDING_CREATE"
	ProvisioningPendingUpdate = "PENDING_UPDATE"
	ProvisioningPendingDelete = "PENDING_DELETE"
)

// ErrNoOctavia means the cloud has no load balancer endpoint. Services of
// type ClusterIP and LoadBalancer cannot be served without one, and saying so
// plainly is better than failing on every call.
var ErrNoOctavia = errors.New("this cloud has no Octavia (load-balancer) endpoint")

// LoadBalancer is an Octavia load balancer.
type LoadBalancer struct {
	ID                 string
	Name               string
	ProjectID          string
	VIPAddress         string
	VIPPortID          string
	VIPSubnetID        string
	Provider           string
	ProvisioningStatus string
	Tags               []string
}

// Ready reports whether the load balancer will accept changes right now.
func (lb *LoadBalancer) Ready() bool {
	return lb.ProvisioningStatus == ProvisioningActive
}

// Listener is an Octavia listener: one protocol and port on the VIP.
type Listener struct {
	ID             string
	Name           string
	ProjectID      string
	LoadBalancerID string
	Protocol       string
	Port           int32
}

// Pool is an Octavia pool: the backends behind one listener.
type Pool struct {
	ID             string
	Name           string
	ProjectID      string
	LoadBalancerID string
	ListenerID     string
	Protocol       string
	Algorithm      string
}

// Member is one backend in a pool.
type Member struct {
	ID       string
	Name     string
	PoolID   string
	SubnetID string
	Address  string
	Port     int32
}

// lb returns the load balancer client, or an error explaining its absence.
func (c *Client) lb() (*gophercloud.ServiceClient, error) {
	if c.LoadBalancer == nil {
		return nil, ErrNoOctavia
	}
	return c.LoadBalancer, nil
}

// HasOctavia reports whether load balancer support is available.
func (c *Client) HasOctavia() bool {
	return c.LoadBalancer != nil
}

// LoadBalancerSpec is what a load balancer is created from.
type LoadBalancerSpec struct {
	Name      string
	ProjectID string
	SubnetID  string
	// VIPAddress is the Service ClusterIP. Octavia allocates one when empty,
	// but Kuryr always knows the address it wants.
	VIPAddress string
	Provider   string
}

// CreateLoadBalancer creates a load balancer and returns it, still building.
//
// Octavia provisions asynchronously, so the result is almost always
// PENDING_CREATE. The caller records the ID and comes back to check.
func (c *Client) CreateLoadBalancer(ctx context.Context, spec LoadBalancerSpec) (*LoadBalancer, error) {
	client, err := c.lb()
	if err != nil {
		return nil, err
	}

	opts := loadbalancers.CreateOpts{
		Name:        spec.Name,
		ProjectID:   spec.ProjectID,
		VipSubnetID: spec.SubnetID,
		VipAddress:  spec.VIPAddress,
		Provider:    spec.Provider,
		Tags:        c.ResourceTags(),
	}

	raw, err := loadbalancers.Create(ctx, client, opts).Extract()
	if err != nil {
		return nil, fmt.Errorf("creating load balancer %q: %w", spec.Name, err)
	}
	return loadBalancerFrom(raw), nil
}

// GetLoadBalancer reads one load balancer.
func (c *Client) GetLoadBalancer(ctx context.Context, id string) (*LoadBalancer, error) {
	client, err := c.lb()
	if err != nil {
		return nil, err
	}

	raw, err := loadbalancers.Get(ctx, client, id).Extract()
	if err != nil {
		return nil, fmt.Errorf("reading load balancer %s: %w", id, err)
	}
	return loadBalancerFrom(raw), nil
}

// FindLoadBalancersByName returns the load balancers with a given name.
//
// The name carries the Service UID, so more than one result means something
// outside Kuryr created a duplicate rather than an ambiguity to resolve.
func (c *Client) FindLoadBalancersByName(ctx context.Context, name, projectID string) ([]*LoadBalancer, error) {
	client, err := c.lb()
	if err != nil {
		return nil, err
	}

	var out []*LoadBalancer
	err = loadbalancers.List(client, loadbalancers.ListOpts{
		Name:      name,
		ProjectID: projectID,
	}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		list, err := loadbalancers.ExtractLoadBalancers(page)
		if err != nil {
			return false, err
		}
		for i := range list {
			out = append(out, loadBalancerFrom(&list[i]))
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing load balancers named %q: %w", name, err)
	}
	return out, nil
}

// DeleteLoadBalancer removes a load balancer and everything inside it.
//
// The cascade is deliberate: listeners, pools and members only exist to serve
// this load balancer, and deleting them one by one would be many round trips
// that Octavia can refuse midway, leaving a half-torn-down tree.
func (c *Client) DeleteLoadBalancer(ctx context.Context, id string) error {
	client, err := c.lb()
	if err != nil {
		return err
	}

	err = loadbalancers.Delete(ctx, client, id, loadbalancers.DeleteOpts{Cascade: true}).ExtractErr()
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("deleting load balancer %s: %w", id, err)
	}
	return nil
}

// CreateListener adds a listener to a load balancer.
func (c *Client) CreateListener(ctx context.Context, l Listener,
	timeoutClientData, timeoutMemberData int32) (*Listener, error) {

	client, err := c.lb()
	if err != nil {
		return nil, err
	}

	opts := listeners.CreateOpts{
		Name:           l.Name,
		ProjectID:      l.ProjectID,
		LoadbalancerID: l.LoadBalancerID,
		Protocol:       listeners.Protocol(l.Protocol),
		ProtocolPort:   int(l.Port),
		Tags:           c.ResourceTags(),
	}
	// Zero means "let Octavia use its own default", which is what leaving the
	// field unset does.
	if timeoutClientData > 0 {
		v := int(timeoutClientData)
		opts.TimeoutClientData = &v
	}
	if timeoutMemberData > 0 {
		v := int(timeoutMemberData)
		opts.TimeoutMemberData = &v
	}

	raw, err := listeners.Create(ctx, client, opts).Extract()
	if err != nil {
		return nil, fmt.Errorf("creating listener %q on load balancer %s: %w",
			l.Name, l.LoadBalancerID, err)
	}
	return listenerFrom(raw), nil
}

// ListListeners returns the listeners of one load balancer.
func (c *Client) ListListeners(ctx context.Context, lbID string) ([]*Listener, error) {
	client, err := c.lb()
	if err != nil {
		return nil, err
	}

	var out []*Listener
	err = listeners.List(client, listeners.ListOpts{LoadbalancerID: lbID}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := listeners.ExtractListeners(page)
			if err != nil {
				return false, err
			}
			for i := range list {
				out = append(out, listenerFrom(&list[i]))
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing listeners of load balancer %s: %w", lbID, err)
	}
	return out, nil
}

// DeleteListener removes a listener. A listener that is already gone is
// reported as success, since that is the state we wanted.
func (c *Client) DeleteListener(ctx context.Context, id string) error {
	client, err := c.lb()
	if err != nil {
		return err
	}
	if err := listeners.Delete(ctx, client, id).ExtractErr(); err != nil && !IsNotFound(err) {
		return fmt.Errorf("deleting listener %s: %w", id, err)
	}
	return nil
}

// CreatePool adds a pool behind a listener.
func (c *Client) CreatePool(ctx context.Context, p Pool) (*Pool, error) {
	client, err := c.lb()
	if err != nil {
		return nil, err
	}

	opts := pools.CreateOpts{
		Name:       p.Name,
		ProjectID:  p.ProjectID,
		ListenerID: p.ListenerID,
		Protocol:   pools.Protocol(p.Protocol),
		LBMethod:   pools.LBMethod(p.Algorithm),
		Tags:       c.ResourceTags(),
	}

	raw, err := pools.Create(ctx, client, opts).Extract()
	if err != nil {
		return nil, fmt.Errorf("creating pool %q for listener %s: %w", p.Name, p.ListenerID, err)
	}
	return poolFrom(raw), nil
}

// ListPools returns the pools of one load balancer.
func (c *Client) ListPools(ctx context.Context, lbID string) ([]*Pool, error) {
	client, err := c.lb()
	if err != nil {
		return nil, err
	}

	var out []*Pool
	err = pools.List(client, pools.ListOpts{LoadbalancerID: lbID}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := pools.ExtractPools(page)
			if err != nil {
				return false, err
			}
			for i := range list {
				out = append(out, poolFrom(&list[i]))
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing pools of load balancer %s: %w", lbID, err)
	}
	return out, nil
}

// DeletePool removes a pool and, with it, its members.
func (c *Client) DeletePool(ctx context.Context, id string) error {
	client, err := c.lb()
	if err != nil {
		return err
	}
	if err := pools.Delete(ctx, client, id).ExtractErr(); err != nil && !IsNotFound(err) {
		return fmt.Errorf("deleting pool %s: %w", id, err)
	}
	return nil
}

// CreateMember adds one backend to a pool.
func (c *Client) CreateMember(ctx context.Context, m Member) (*Member, error) {
	client, err := c.lb()
	if err != nil {
		return nil, err
	}

	opts := pools.CreateMemberOpts{
		Name:         m.Name,
		Address:      m.Address,
		ProtocolPort: int(m.Port),
		SubnetID:     m.SubnetID,
		Tags:         c.ResourceTags(),
	}

	raw, err := pools.CreateMember(ctx, client, m.PoolID, opts).Extract()
	if err != nil {
		return nil, fmt.Errorf("adding member %s:%d to pool %s: %w",
			m.Address, m.Port, m.PoolID, err)
	}
	return memberFrom(m.PoolID, raw), nil
}

// ListMembers returns the members of one pool.
func (c *Client) ListMembers(ctx context.Context, poolID string) ([]*Member, error) {
	client, err := c.lb()
	if err != nil {
		return nil, err
	}

	var out []*Member
	err = pools.ListMembers(client, poolID, pools.ListMembersOpts{}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := pools.ExtractMembers(page)
			if err != nil {
				return false, err
			}
			for i := range list {
				out = append(out, memberFrom(poolID, &list[i]))
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing members of pool %s: %w", poolID, err)
	}
	return out, nil
}

// DeleteMember removes one backend from a pool.
func (c *Client) DeleteMember(ctx context.Context, poolID, memberID string) error {
	client, err := c.lb()
	if err != nil {
		return err
	}
	err = pools.DeleteMember(ctx, client, poolID, memberID).ExtractErr()
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("removing member %s from pool %s: %w", memberID, poolID, err)
	}
	return nil
}

// ListProviders returns the Octavia provider drivers this cloud offers.
//
// It is used to check a configured provider exists before building anything
// with it, since Octavia only rejects an unknown provider at create time and
// the error is not obvious.
func (c *Client) ListProviders(ctx context.Context) ([]string, error) {
	client, err := c.lb()
	if err != nil {
		return nil, err
	}

	var out []string
	err = providers.List(client, providers.ListOpts{}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := providers.ExtractProviders(page)
			if err != nil {
				return false, err
			}
			for i := range list {
				out = append(out, list[i].Name)
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing Octavia providers: %w", err)
	}
	return out, nil
}

func loadBalancerFrom(raw *loadbalancers.LoadBalancer) *LoadBalancer {
	return &LoadBalancer{
		ID:                 raw.ID,
		Name:               raw.Name,
		ProjectID:          raw.ProjectID,
		VIPAddress:         raw.VipAddress,
		VIPPortID:          raw.VipPortID,
		VIPSubnetID:        raw.VipSubnetID,
		Provider:           raw.Provider,
		ProvisioningStatus: raw.ProvisioningStatus,
		Tags:               raw.Tags,
	}
}

func listenerFrom(raw *listeners.Listener) *Listener {
	l := &Listener{
		ID:             raw.ID,
		Name:           raw.Name,
		ProjectID:      raw.ProjectID,
		Protocol:       raw.Protocol,
		Port:           int32(raw.ProtocolPort),
		LoadBalancerID: "",
	}
	// A listener belongs to exactly one load balancer, but the API returns
	// the relation as a list.
	if len(raw.Loadbalancers) > 0 {
		l.LoadBalancerID = raw.Loadbalancers[0].ID
	}
	return l
}

func poolFrom(raw *pools.Pool) *Pool {
	p := &Pool{
		ID:        raw.ID,
		Name:      raw.Name,
		ProjectID: raw.ProjectID,
		Protocol:  raw.Protocol,
		Algorithm: raw.LBMethod,
	}
	if len(raw.Listeners) > 0 {
		p.ListenerID = raw.Listeners[0].ID
	}
	if len(raw.Loadbalancers) > 0 {
		p.LoadBalancerID = raw.Loadbalancers[0].ID
	}
	return p
}

func memberFrom(poolID string, raw *pools.Member) *Member {
	return &Member{
		ID:       raw.ID,
		Name:     raw.Name,
		PoolID:   poolID,
		SubnetID: raw.SubnetID,
		Address:  raw.Address,
		Port:     int32(raw.ProtocolPort),
	}
}
