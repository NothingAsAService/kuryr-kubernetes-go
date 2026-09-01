package openstack

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// FloatingIP is a Neutron floating IP.
type FloatingIP struct {
	ID         string
	Address    string
	NetworkID  string
	PortID     string
	ProjectID  string
	Status     string
	Descriptor string
}

// FloatingIPSpec is what a floating IP is allocated from.
type FloatingIPSpec struct {
	// NetworkID is the external network to allocate from.
	NetworkID string
	// SubnetID narrows the allocation to one subnet of that network.
	SubnetID string
	// ProjectID owns the address.
	ProjectID string
	// PortID, when set, associates the address as part of the allocation,
	// which saves a round trip and leaves no window where the address exists
	// but points nowhere.
	PortID      string
	Description string
}

// AllocateFloatingIP takes an address from an external network.
func (c *Client) AllocateFloatingIP(ctx context.Context, spec FloatingIPSpec) (*FloatingIP, error) {
	opts := floatingips.CreateOpts{
		FloatingNetworkID: spec.NetworkID,
		SubnetID:          spec.SubnetID,
		ProjectID:         spec.ProjectID,
		PortID:            spec.PortID,
		Description:       spec.Description,
	}

	raw, err := floatingips.Create(ctx, c.Network, opts).Extract()
	if err != nil {
		return nil, fmt.Errorf("allocating a floating IP on network %s: %w", spec.NetworkID, err)
	}
	return floatingIPFrom(raw), nil
}

// GetFloatingIP reads one floating IP.
func (c *Client) GetFloatingIP(ctx context.Context, id string) (*FloatingIP, error) {
	raw, err := floatingips.Get(ctx, c.Network, id).Extract()
	if err != nil {
		return nil, fmt.Errorf("reading floating IP %s: %w", id, err)
	}
	return floatingIPFrom(raw), nil
}

// FindFloatingIPByAddress locates a floating IP by its address.
//
// This is how a Service that names an address in spec.loadBalancerIP is
// honoured: the address must already exist as a floating IP in the project,
// because Kuryr will not invent one outside the configured pool.
func (c *Client) FindFloatingIPByAddress(ctx context.Context, address string) (*FloatingIP, error) {
	var found *FloatingIP
	err := floatingips.List(c.Network, floatingips.ListOpts{FloatingIP: address}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := floatingips.ExtractFloatingIPs(page)
			if err != nil {
				return false, err
			}
			if len(list) > 0 {
				found = floatingIPFrom(&list[0])
				return false, nil
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("looking up floating IP %s: %w", address, err)
	}
	return found, nil
}

// AssociateFloatingIP points a floating IP at a port.
func (c *Client) AssociateFloatingIP(ctx context.Context, id, portID string) error {
	_, err := floatingips.Update(ctx, c.Network, id, floatingips.UpdateOpts{PortID: &portID}).Extract()
	if err != nil {
		return fmt.Errorf("associating floating IP %s with port %s: %w", id, portID, err)
	}
	return nil
}

// DisassociateFloatingIP detaches a floating IP from whatever port it points
// at, leaving the address allocated.
func (c *Client) DisassociateFloatingIP(ctx context.Context, id string) error {
	empty := ""
	_, err := floatingips.Update(ctx, c.Network, id, floatingips.UpdateOpts{PortID: &empty}).Extract()
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("disassociating floating IP %s: %w", id, err)
	}
	return nil
}

// DeleteFloatingIP releases an address back to the pool. An address already
// gone is reported as success.
func (c *Client) DeleteFloatingIP(ctx context.Context, id string) error {
	if err := floatingips.Delete(ctx, c.Network, id).ExtractErr(); err != nil && !IsNotFound(err) {
		return fmt.Errorf("releasing floating IP %s: %w", id, err)
	}
	return nil
}

func floatingIPFrom(raw *floatingips.FloatingIP) *FloatingIP {
	return &FloatingIP{
		ID:         raw.ID,
		Address:    raw.FloatingIP,
		NetworkID:  raw.FloatingNetworkID,
		PortID:     raw.PortID,
		ProjectID:  raw.ProjectID,
		Status:     raw.Status,
		Descriptor: raw.Description,
	}
}
