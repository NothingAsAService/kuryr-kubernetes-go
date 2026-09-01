package openstack

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/attributestags"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/mtu"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// Subnet is a Neutron subnet reduced to what a VIF needs.
type Subnet struct {
	ID          string
	NetworkID   string
	Name        string
	CIDR        string
	GatewayIP   string
	IPVersion   int
	DNS         []string
	HostRoutes  []HostRoute
	EnableDHCP  bool
	SubnetPool  string
	Description string
}

// HostRoute is a static route advertised by a subnet.
type HostRoute struct {
	Destination string
	NextHop     string
}

// Network is a Neutron network reduced to what a VIF needs.
type Network struct {
	ID          string
	Name        string
	MTU         int
	Description string
	Subnets     []string
	Tags        []string
}

// resourceCache memoises subnet and network lookups.
//
// Every pod creation needs the subnet and network behind its port, and those
// change far more slowly than pods are created. The Python implementation used
// oslo.cache with a dogpile backend for the same reason; this is the same idea
// with one fewer dependency.
type resourceCache[T any] struct {
	mu    sync.RWMutex
	ttl   time.Duration
	items map[string]cacheEntry[T]
}

type cacheEntry[T any] struct {
	value   T
	expires time.Time
}

func newResourceCache[T any](ttl time.Duration) *resourceCache[T] {
	return &resourceCache[T]{ttl: ttl, items: map[string]cacheEntry[T]{}}
}

func (c *resourceCache[T]) get(key string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[key]
	if !ok || time.Now().After(e.expires) {
		var zero T
		return zero, false
	}
	return e.value, true
}

func (c *resourceCache[T]) put(key string, value T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = cacheEntry[T]{value: value, expires: time.Now().Add(c.ttl)}
}

func (c *resourceCache[T]) invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

const resourceCacheTTL = 5 * time.Minute

var (
	subnetCache  = newResourceCache[*Subnet](resourceCacheTTL)
	networkCache = newResourceCache[*Network](resourceCacheTTL)
)

// GetSubnet fetches a subnet, from cache when possible.
func (c *Client) GetSubnet(ctx context.Context, id string) (*Subnet, error) {
	if s, ok := subnetCache.get(id); ok {
		return s, nil
	}

	raw, err := subnets.Get(ctx, c.Network, id).Extract()
	if err != nil {
		return nil, fmt.Errorf("getting subnet %s: %w", id, err)
	}

	s := &Subnet{
		ID:          raw.ID,
		NetworkID:   raw.NetworkID,
		Name:        raw.Name,
		CIDR:        raw.CIDR,
		GatewayIP:   raw.GatewayIP,
		IPVersion:   raw.IPVersion,
		DNS:         raw.DNSNameservers,
		EnableDHCP:  raw.EnableDHCP,
		SubnetPool:  raw.SubnetPoolID,
		Description: raw.Description,
	}
	for _, r := range raw.HostRoutes {
		s.HostRoutes = append(s.HostRoutes, HostRoute{Destination: r.DestinationCIDR, NextHop: r.NextHop})
	}

	subnetCache.put(id, s)
	return s, nil
}

// GetNetwork fetches a network, from cache when possible.
func (c *Client) GetNetwork(ctx context.Context, id string) (*Network, error) {
	if n, ok := networkCache.get(id); ok {
		return n, nil
	}

	var raw networkWithMTU
	if err := networks.Get(ctx, c.Network, id).ExtractInto(&raw); err != nil {
		return nil, fmt.Errorf("getting network %s: %w", id, err)
	}

	n := &Network{
		ID:          raw.ID,
		Name:        raw.Name,
		MTU:         raw.MTU,
		Description: raw.Description,
		Subnets:     raw.Subnets,
		Tags:        raw.Tags,
	}
	networkCache.put(id, n)
	return n, nil
}

// InvalidateNetwork drops a network from the cache, for use after Kuryr
// itself changes one.
func (c *Client) InvalidateNetwork(id string) { networkCache.invalidate(id) }

// InvalidateSubnet drops a subnet from the cache.
func (c *Client) InvalidateSubnet(id string) { subnetCache.invalidate(id) }

// CreateNetworkSpec describes a network to create for a namespace.
type CreateNetworkSpec struct {
	Name        string
	ProjectID   string
	Description string
	Tags        []string
}

// CreateNetwork creates a Neutron network.
func (c *Client) CreateNetwork(ctx context.Context, spec CreateNetworkSpec) (*Network, error) {
	up := true
	var raw networkWithMTU
	if err := networks.Create(ctx, c.Network, networks.CreateOpts{
		Name:         spec.Name,
		Description:  spec.Description,
		AdminStateUp: &up,
		ProjectID:    spec.ProjectID,
	}).ExtractInto(&raw); err != nil {
		return nil, fmt.Errorf("creating network %s: %w", spec.Name, err)
	}

	tags := spec.Tags
	if len(tags) == 0 {
		tags = c.Tags
	}
	if len(tags) > 0 {
		if err := c.TagResource(ctx, "networks", raw.ID, tags); err != nil {
			return nil, err
		}
	}

	return &Network{ID: raw.ID, Name: raw.Name, MTU: raw.MTU, Description: raw.Description}, nil
}

// DeleteNetwork removes a network, treating an absent one as success.
func (c *Client) DeleteNetwork(ctx context.Context, id string) error {
	err := networks.Delete(ctx, c.Network, id).ExtractErr()
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("deleting network %s: %w", id, err)
	}
	networkCache.invalidate(id)
	return nil
}

// FindNetworksByName lists networks matching any of the given names, which is
// how namespace teardown rediscovers a network whose ID was never recorded.
func (c *Client) FindNetworksByName(ctx context.Context, names ...string) ([]*Network, error) {
	var out []*Network
	for _, name := range names {
		err := networks.List(c.Network, networks.ListOpts{Name: name}).EachPage(ctx,
			func(_ context.Context, page pagination.Page) (bool, error) {
				var list []networkWithMTU
				if err := networks.ExtractNetworksInto(page, &list); err != nil {
					return false, err
				}
				for _, n := range list {
					out = append(out, &Network{
						ID: n.ID, Name: n.Name, MTU: n.MTU,
						Description: n.Description, Subnets: n.Subnets, Tags: n.Tags,
					})
				}
				return true, nil
			})
		if err != nil {
			return nil, fmt.Errorf("listing networks named %s: %w", name, err)
		}
	}
	return out, nil
}

// CreateSubnetSpec describes a subnet to create for a namespace.
type CreateSubnetSpec struct {
	Name         string
	NetworkID    string
	ProjectID    string
	SubnetPoolID string
	CIDR         string
	IPVersion    int
	Tags         []string
}

// CreateSubnet creates a Neutron subnet, allocating from a pool when no
// explicit CIDR is given.
func (c *Client) CreateSubnet(ctx context.Context, spec CreateSubnetSpec) (*Subnet, error) {
	version := spec.IPVersion
	if version == 0 {
		version = 4
	}
	opts := subnets.CreateOpts{
		Name:      spec.Name,
		NetworkID: spec.NetworkID,
		ProjectID: spec.ProjectID,
		IPVersion: gophercloud.IPVersion(version),
	}
	if spec.SubnetPoolID != "" {
		opts.SubnetPoolID = spec.SubnetPoolID
	}
	if spec.CIDR != "" {
		opts.CIDR = spec.CIDR
	}

	raw, err := subnets.Create(ctx, c.Network, opts).Extract()
	if err != nil {
		return nil, fmt.Errorf("creating subnet %s on network %s: %w",
			spec.Name, spec.NetworkID, err)
	}

	tags := spec.Tags
	if len(tags) == 0 {
		tags = c.Tags
	}
	if len(tags) > 0 {
		if err := c.TagResource(ctx, "subnets", raw.ID, tags); err != nil {
			return nil, err
		}
	}

	return &Subnet{
		ID: raw.ID, NetworkID: raw.NetworkID, Name: raw.Name,
		CIDR: raw.CIDR, GatewayIP: raw.GatewayIP, IPVersion: raw.IPVersion,
		DNS: raw.DNSNameservers, SubnetPool: raw.SubnetPoolID,
	}, nil
}

// DeleteSubnet removes a subnet, treating an absent one as success.
func (c *Client) DeleteSubnet(ctx context.Context, id string) error {
	err := subnets.Delete(ctx, c.Network, id).ExtractErr()
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("deleting subnet %s: %w", id, err)
	}
	subnetCache.invalidate(id)
	return nil
}

// TagResource replaces the tags on any taggable Neutron resource.
func (c *Client) TagResource(ctx context.Context, resourceType, id string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	err := attributestags.ReplaceAll(ctx, c.Network, resourceType, id,
		attributestags.ReplaceAllOpts{Tags: tags}).Err
	if err != nil {
		return fmt.Errorf("tagging %s %s: %w", resourceType, id, err)
	}
	return nil
}

// networkWithMTU is a network plus the MTU extension. The nested binding
// driver refuses to plug a VIF whose network MTU exceeds the node uplink MTU,
// so this field is load bearing rather than informational.
type networkWithMTU struct {
	networks.Network
	mtu.NetworkMTUExt
}
