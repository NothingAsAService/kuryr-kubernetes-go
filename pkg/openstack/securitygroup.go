package openstack

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// SecurityGroup is a Neutron security group.
type SecurityGroup struct {
	ID          string
	Name        string
	ProjectID   string
	Description string
	Tags        []string
}

// SGRule is one rule inside a security group.
//
// Field names follow Kubernetes conventions rather than Neutron's wire names;
// the translation happens here so nothing above this package has to know that
// Neutron calls it remote_ip_prefix.
type SGRule struct {
	ID              string
	SecurityGroupID string
	Direction       string
	EtherType       string
	Protocol        string
	PortRangeMin    int
	PortRangeMax    int
	RemoteIPPrefix  string
	Description     string
}

// CreateSecurityGroup creates a group and tags it.
func (c *Client) CreateSecurityGroup(ctx context.Context, name, projectID, description string) (*SecurityGroup, error) {
	raw, err := groups.Create(ctx, c.Network, groups.CreateOpts{
		Name:        name,
		Description: description,
		ProjectID:   projectID,
	}).Extract()
	if err != nil {
		return nil, fmt.Errorf("creating security group %s: %w", name, err)
	}

	sg := &SecurityGroup{
		ID:          raw.ID,
		Name:        raw.Name,
		ProjectID:   raw.ProjectID,
		Description: raw.Description,
	}

	if len(c.Tags) > 0 {
		if err := c.TagResource(ctx, "security-groups", sg.ID, c.Tags); err != nil {
			// An untagged group would be invisible to garbage collection, so
			// this is worth undoing rather than leaving behind.
			if delErr := c.DeleteSecurityGroup(ctx, sg.ID); delErr != nil {
				return nil, fmt.Errorf("tagging security group %s failed (%w), "+
					"and deleting it also failed: %v", sg.ID, err, delErr)
			}
			return nil, err
		}
		sg.Tags = c.Tags
	}
	return sg, nil
}

// GetSecurityGroup reads one group.
func (c *Client) GetSecurityGroup(ctx context.Context, id string) (*SecurityGroup, error) {
	raw, err := groups.Get(ctx, c.Network, id).Extract()
	if err != nil {
		return nil, fmt.Errorf("reading security group %s: %w", id, err)
	}
	return &SecurityGroup{
		ID:          raw.ID,
		Name:        raw.Name,
		ProjectID:   raw.ProjectID,
		Description: raw.Description,
		Tags:        raw.Tags,
	}, nil
}

// DeleteSecurityGroup removes a group, treating an absent one as success.
func (c *Client) DeleteSecurityGroup(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	err := groups.Delete(ctx, c.Network, id).ExtractErr()
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("deleting security group %s: %w", id, err)
	}
	return nil
}

// FindSecurityGroupsByName lists groups with a given name, which is how a
// controller restart rediscovers a group whose ID was never recorded.
func (c *Client) FindSecurityGroupsByName(ctx context.Context, name, projectID string) ([]*SecurityGroup, error) {
	opts := groups.ListOpts{Name: name}
	if projectID != "" {
		opts.ProjectID = projectID
	}

	var out []*SecurityGroup
	err := groups.List(c.Network, opts).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := groups.ExtractGroups(page)
			if err != nil {
				return false, err
			}
			for _, g := range list {
				out = append(out, &SecurityGroup{
					ID: g.ID, Name: g.Name, ProjectID: g.ProjectID,
					Description: g.Description, Tags: g.Tags,
				})
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing security groups named %s: %w", name, err)
	}
	return out, nil
}

// CreateSGRule adds one rule to a group.
//
// A rule identical to one already present makes Neutron return a conflict.
// That means the desired state is already true, so it is reported as success
// and the existing rule is looked up so the caller still gets an ID.
func (c *Client) CreateSGRule(ctx context.Context, r SGRule) (*SGRule, error) {
	opts := rules.CreateOpts{
		SecGroupID:     r.SecurityGroupID,
		Direction:      rules.RuleDirection(r.Direction),
		EtherType:      rules.RuleEtherType(r.EtherType),
		Description:    r.Description,
		RemoteIPPrefix: r.RemoteIPPrefix,
	}
	if r.Protocol != "" {
		opts.Protocol = rules.RuleProtocol(r.Protocol)
	}
	if r.PortRangeMin > 0 {
		opts.PortRangeMin = r.PortRangeMin
	}
	if r.PortRangeMax > 0 {
		opts.PortRangeMax = r.PortRangeMax
	}

	raw, err := rules.Create(ctx, c.Network, opts).Extract()
	if err != nil {
		if IsConflict(err) {
			existing, findErr := c.findMatchingRule(ctx, r)
			if findErr != nil {
				return nil, fmt.Errorf("rule already exists in group %s but could not be "+
					"looked up: %w", r.SecurityGroupID, findErr)
			}
			if existing != nil {
				return existing, nil
			}
		}
		return nil, fmt.Errorf("creating security group rule in %s: %w", r.SecurityGroupID, err)
	}

	out := r
	out.ID = raw.ID
	return &out, nil
}

// findMatchingRule locates a rule equal to the one we tried to create.
func (c *Client) findMatchingRule(ctx context.Context, want SGRule) (*SGRule, error) {
	existing, err := c.ListSGRules(ctx, want.SecurityGroupID)
	if err != nil {
		return nil, err
	}
	for _, r := range existing {
		if r.Direction == want.Direction &&
			r.EtherType == want.EtherType &&
			r.Protocol == want.Protocol &&
			r.PortRangeMin == want.PortRangeMin &&
			r.PortRangeMax == want.PortRangeMax &&
			r.RemoteIPPrefix == want.RemoteIPPrefix {
			return r, nil
		}
	}
	return nil, nil
}

// ListSGRules returns every rule in a group.
func (c *Client) ListSGRules(ctx context.Context, sgID string) ([]*SGRule, error) {
	var out []*SGRule
	err := rules.List(c.Network, rules.ListOpts{SecGroupID: sgID}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := rules.ExtractRules(page)
			if err != nil {
				return false, err
			}
			for _, r := range list {
				out = append(out, &SGRule{
					ID:              r.ID,
					SecurityGroupID: r.SecGroupID,
					Direction:       r.Direction,
					EtherType:       r.EtherType,
					Protocol:        r.Protocol,
					PortRangeMin:    r.PortRangeMin,
					PortRangeMax:    r.PortRangeMax,
					RemoteIPPrefix:  r.RemoteIPPrefix,
					Description:     r.Description,
				})
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing rules of security group %s: %w", sgID, err)
	}
	return out, nil
}

// DeleteSGRule removes one rule, treating an absent one as success.
func (c *Client) DeleteSGRule(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	err := rules.Delete(ctx, c.Network, id).ExtractErr()
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("deleting security group rule %s: %w", id, err)
	}
	return nil
}
