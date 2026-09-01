package openstack

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/trunks"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// SegmentationTypeVLAN is the only trunk segmentation type Kuryr uses.
const SegmentationTypeVLAN = "vlan"

// Trunk is a Neutron trunk port: the single port a node VM owns, through
// which every pod subport is multiplexed.
type Trunk struct {
	ID           string
	Name         string
	PortID       string
	ProjectID    string
	SubPorts     []SubPort
	Tags         []string
	Description  string
	AdminStateUp bool
}

// SubPort is one pod port riding on a trunk under a VLAN tag.
type SubPort struct {
	PortID           string
	SegmentationID   int
	SegmentationType string
}

// UsedVLANs returns the segmentation ids currently taken on this trunk.
func (t *Trunk) UsedVLANs() map[int]bool {
	used := make(map[int]bool, len(t.SubPorts))
	for _, sp := range t.SubPorts {
		used[sp.SegmentationID] = true
	}
	return used
}

func toTrunk(raw *trunks.Trunk) *Trunk {
	t := &Trunk{
		ID:           raw.ID,
		Name:         raw.Name,
		PortID:       raw.PortID,
		ProjectID:    raw.ProjectID,
		Tags:         raw.Tags,
		Description:  raw.Description,
		AdminStateUp: raw.AdminStateUp,
	}
	for _, sp := range raw.Subports {
		t.SubPorts = append(t.SubPorts, SubPort{
			PortID:           sp.PortID,
			SegmentationID:   sp.SegmentationID,
			SegmentationType: sp.SegmentationType,
		})
	}
	return t
}

// GetTrunk fetches one trunk by id.
func (c *Client) GetTrunk(ctx context.Context, id string) (*Trunk, error) {
	raw, err := trunks.Get(ctx, c.Network, id).Extract()
	if err != nil {
		return nil, fmt.Errorf("getting trunk %s: %w", id, err)
	}
	return toTrunk(raw), nil
}

// FindTrunkByParentPort locates the trunk whose parent is the given port.
//
// This is the lookup that turns a node VM into a place pods can be attached:
// the VM has exactly one trunk, and every pod on that node becomes a subport
// of it.
func (c *Client) FindTrunkByParentPort(ctx context.Context, portID string) (*Trunk, error) {
	var found *Trunk
	err := trunks.List(c.Network, trunks.ListOpts{PortID: portID}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := trunks.ExtractTrunks(page)
			if err != nil {
				return false, err
			}
			if len(list) > 0 {
				found = toTrunk(&list[0])
				return false, nil
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing trunks for parent port %s: %w", portID, err)
	}
	if found == nil {
		return nil, fmt.Errorf("no trunk found for parent port %s: "+
			"is the node VM booted with a trunk port", portID)
	}
	return found, nil
}

// ListTrunks returns every trunk carrying Kuryr tags.
func (c *Client) ListTrunks(ctx context.Context) ([]*Trunk, error) {
	opts := trunks.ListOpts{}
	if len(c.Tags) > 0 {
		opts.Tags = joinTags(c.Tags)
	}

	var out []*Trunk
	err := trunks.List(c.Network, opts).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := trunks.ExtractTrunks(page)
			if err != nil {
				return false, err
			}
			for i := range list {
				out = append(out, toTrunk(&list[i]))
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing trunks: %w", err)
	}
	return out, nil
}

// AddSubPorts attaches ports to a trunk.
//
// Neutron applies the whole batch atomically: either every subport is
// attached or none is. That matters for the pool driver, which allocates VLAN
// ids for a batch up front and must be able to hand them all back on failure.
func (c *Client) AddSubPorts(ctx context.Context, trunkID string, subports []SubPort) error {
	if len(subports) == 0 {
		return nil
	}
	opts := trunks.AddSubportsOpts{Subports: make([]trunks.Subport, 0, len(subports))}
	for _, sp := range subports {
		segType := sp.SegmentationType
		if segType == "" {
			segType = SegmentationTypeVLAN
		}
		opts.Subports = append(opts.Subports, trunks.Subport{
			PortID:           sp.PortID,
			SegmentationID:   sp.SegmentationID,
			SegmentationType: segType,
		})
	}
	if _, err := trunks.AddSubports(ctx, c.Network, trunkID, opts).Extract(); err != nil {
		return fmt.Errorf("attaching %d subports to trunk %s: %w", len(subports), trunkID, err)
	}
	return nil
}

// RemoveSubPorts detaches ports from a trunk. A subport that is already gone
// is not an error, since every teardown path may be retried.
func (c *Client) RemoveSubPorts(ctx context.Context, trunkID string, portIDs []string) error {
	if len(portIDs) == 0 {
		return nil
	}
	opts := trunks.RemoveSubportsOpts{Subports: make([]trunks.RemoveSubport, 0, len(portIDs))}
	for _, id := range portIDs {
		opts.Subports = append(opts.Subports, trunks.RemoveSubport{PortID: id})
	}
	if _, err := trunks.RemoveSubports(ctx, c.Network, trunkID, opts).Extract(); err != nil {
		if IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("detaching subports from trunk %s: %w", trunkID, err)
	}
	return nil
}
