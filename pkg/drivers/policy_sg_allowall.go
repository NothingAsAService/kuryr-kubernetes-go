package drivers

import (
	"context"
	"fmt"
	"sync"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

// Names of the two shared groups. They carry the direction rather than a
// policy identity because they are shared by every pod that needs them, and
// an operator reading a Neutron listing should be able to tell at a glance
// that these are not derived from any one NetworkPolicy.
const (
	allowAllIngressGroupName = "kuryr-allow-all-ingress"
	allowAllEgressGroupName  = "kuryr-allow-all-egress"
)

// allowAllCloud is the slice of Neutron the shared groups need.
//
// It is an interface so the policy driver can be tested without a cloud;
// *openstack.Client satisfies it.
type allowAllCloud interface {
	FindSecurityGroupsByName(ctx context.Context, name, projectID string) ([]*openstack.SecurityGroup, error)
	CreateSecurityGroup(ctx context.Context, name, projectID, description string) (*openstack.SecurityGroup, error)
	ListSGRules(ctx context.Context, sgID string) ([]*openstack.SGRule, error)
	CreateSGRule(ctx context.Context, r openstack.SGRule) (*openstack.SGRule, error)
	DeleteSGRule(ctx context.Context, id string) error
}

// allowAllGroups hands out the security group that leaves one direction
// completely open, creating it on first use and remembering it afterwards.
//
// There is one group per direction per OpenStack project. Per project,
// because a pod's groups have to live in the same project as its port, and a
// project driver may put different namespaces in different projects.
type allowAllGroups struct {
	os allowAllCloud

	// mu guards ids. Reconcilers run concurrently, and two of them asking for
	// the same group at the same time must not create two groups.
	mu  sync.Mutex
	ids map[string]string
}

func newAllowAllGroups(os allowAllCloud) *allowAllGroups {
	return &allowAllGroups{os: os, ids: map[string]string{}}
}

// id returns the group that opens direction for the given project.
func (a *allowAllGroups) id(ctx context.Context, projectID string, direction kuryrv1.SGDirection) (string, error) {
	name, err := allowAllGroupName(direction)
	if err != nil {
		return "", err
	}

	// The whole ensure runs under the lock. It is a handful of Neutron calls
	// once per project per process, and serialising it is what stops two
	// reconcilers from each creating a group.
	a.mu.Lock()
	defer a.mu.Unlock()

	key := projectID + "/" + name
	if id, ok := a.ids[key]; ok {
		return id, nil
	}

	id, err := a.ensure(ctx, name, projectID, direction)
	if err != nil {
		return "", err
	}
	a.ids[key] = id
	return id, nil
}

// ensure finds or creates the group and makes its rules exactly right.
func (a *allowAllGroups) ensure(ctx context.Context, name, projectID string,
	direction kuryrv1.SGDirection) (string, error) {

	existing, err := a.os.FindSecurityGroupsByName(ctx, name, projectID)
	if err != nil {
		return "", fmt.Errorf("looking for the security group named %q: %w", name, err)
	}

	var sgID string
	switch len(existing) {
	case 0:
		description := fmt.Sprintf(
			"kuryr: allows all %s, for pods no NetworkPolicy restricts in that direction", direction)
		sg, err := a.os.CreateSecurityGroup(ctx, name, projectID, description)
		if err != nil {
			return "", fmt.Errorf("creating the security group named %q: %w", name, err)
		}
		sgID = sg.ID
	case 1:
		sgID = existing[0].ID
	default:
		// Picking one arbitrarily would leave the others attached to some
		// ports and not others, which is exactly the kind of divergence that
		// is impossible to debug later.
		return "", fmt.Errorf("found %d security groups named %q in project %q; expected at most one, "+
			"remove the duplicates before continuing", len(existing), name, projectID)
	}

	if err := a.reconcileRules(ctx, sgID, direction); err != nil {
		return "", err
	}
	return sgID, nil
}

// reconcileRules makes the group hold exactly two rules: allow everything in
// the one direction, for each address family.
//
// Both halves matter. The missing rules have to be added because an adopted
// group may have been created and then abandoned half-built. The extra rules
// have to go because Neutron seeds every new group with two egress
// allow-alls: leaving those on the ingress group would mean a pod whose
// egress *is* restricted has it opened again the moment it needs the ingress
// group, which is the very mistake this whole mechanism exists to avoid.
func (a *allowAllGroups) reconcileRules(ctx context.Context, sgID string, direction kuryrv1.SGDirection) error {
	have, err := a.os.ListSGRules(ctx, sgID)
	if err != nil {
		return fmt.Errorf("listing the rules of security group %s: %w", sgID, err)
	}

	wanted := map[string]openstack.SGRule{}
	for _, etherType := range []kuryrv1.SGEtherType{kuryrv1.SGEtherTypeIPv4, kuryrv1.SGEtherTypeIPv6} {
		rule := openstack.SGRule{
			SecurityGroupID: sgID,
			Direction:       string(direction),
			EtherType:       string(etherType),
		}
		wanted[allowAllRuleKey(rule)] = rule
	}

	for _, rule := range have {
		key := allowAllRuleKey(*rule)
		if _, ok := wanted[key]; ok {
			delete(wanted, key)
			continue
		}
		if err := a.os.DeleteSGRule(ctx, rule.ID); err != nil && !openstack.IsNotFound(err) {
			return fmt.Errorf("removing rule %s from security group %s: %w", rule.ID, sgID, err)
		}
	}

	for _, rule := range wanted {
		if _, err := a.os.CreateSGRule(ctx, rule); err != nil {
			return fmt.Errorf("adding the allow-all %s rule to security group %s: %w",
				direction, sgID, err)
		}
	}
	return nil
}

// allowAllRuleKey identifies a rule by what it matches, so a rule read back
// from Neutron lines up with the one that was asked for.
func allowAllRuleKey(r openstack.SGRule) string {
	return fmt.Sprintf("%s|%s|%s|%d|%d|%s",
		r.Direction, r.EtherType, r.Protocol, r.PortRangeMin, r.PortRangeMax, r.RemoteIPPrefix)
}

func allowAllGroupName(direction kuryrv1.SGDirection) (string, error) {
	switch direction {
	case kuryrv1.SGDirectionIngress:
		return allowAllIngressGroupName, nil
	case kuryrv1.SGDirectionEgress:
		return allowAllEgressGroupName, nil
	default:
		return "", fmt.Errorf("no allow-all security group exists for direction %q", direction)
	}
}
