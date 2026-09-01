package drivers

import (
	"context"
	"fmt"
	"testing"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

// fakeAllowAllCloud is enough Neutron to exercise the shared allow-all
// groups: it remembers groups by name and rules by group, which is all the
// driver ever asks about.
type fakeAllowAllCloud struct {
	groups []*openstack.SecurityGroup
	rules  map[string][]*openstack.SGRule

	nextID  int
	creates int // groups created, so a test can prove caching works
	failNow error
}

func newFakeAllowAllCloud() *fakeAllowAllCloud {
	return &fakeAllowAllCloud{rules: map[string][]*openstack.SGRule{}}
}

func (f *fakeAllowAllCloud) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeAllowAllCloud) FindSecurityGroupsByName(_ context.Context, name,
	projectID string) ([]*openstack.SecurityGroup, error) {

	if f.failNow != nil {
		return nil, f.failNow
	}
	var out []*openstack.SecurityGroup
	for _, g := range f.groups {
		if g.Name == name && (projectID == "" || g.ProjectID == projectID) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeAllowAllCloud) CreateSecurityGroup(_ context.Context, name, projectID,
	description string) (*openstack.SecurityGroup, error) {

	if f.failNow != nil {
		return nil, f.failNow
	}
	sg := &openstack.SecurityGroup{
		ID: f.id("sg"), Name: name, ProjectID: projectID, Description: description,
	}
	f.groups = append(f.groups, sg)
	f.creates++

	// Neutron seeds every new group with egress allow-alls. Reproducing that
	// is the point: the driver has to remove them from the ingress group.
	for _, etherType := range []string{"IPv4", "IPv6"} {
		f.rules[sg.ID] = append(f.rules[sg.ID], &openstack.SGRule{
			ID: f.id("rule"), SecurityGroupID: sg.ID, Direction: "egress", EtherType: etherType,
		})
	}
	return sg, nil
}

func (f *fakeAllowAllCloud) ListSGRules(_ context.Context, sgID string) ([]*openstack.SGRule, error) {
	if f.failNow != nil {
		return nil, f.failNow
	}
	return append([]*openstack.SGRule(nil), f.rules[sgID]...), nil
}

func (f *fakeAllowAllCloud) CreateSGRule(_ context.Context, r openstack.SGRule) (*openstack.SGRule, error) {
	if f.failNow != nil {
		return nil, f.failNow
	}
	r.ID = f.id("rule")
	stored := r
	f.rules[r.SecurityGroupID] = append(f.rules[r.SecurityGroupID], &stored)
	return &stored, nil
}

func (f *fakeAllowAllCloud) DeleteSGRule(_ context.Context, id string) error {
	if f.failNow != nil {
		return f.failNow
	}
	for sgID, list := range f.rules {
		for i, r := range list {
			if r.ID == id {
				f.rules[sgID] = append(list[:i], list[i+1:]...)
				return nil
			}
		}
	}
	return nil
}

// ruleSet renders a group's rules as direction/ethertype pairs, which is the
// only thing that distinguishes the rules these groups hold.
func (f *fakeAllowAllCloud) ruleSet(sgID string) map[string]bool {
	out := map[string]bool{}
	for _, r := range f.rules[sgID] {
		out[r.Direction+"/"+r.EtherType] = true
	}
	return out
}

func TestAllowAllIngressGroupCarriesNoEgressRules(t *testing.T) {
	// Neutron creates a new group with two egress allow-alls. Leaving them on
	// the ingress group would open egress for every pod that needs it, which
	// is the exact widening this mechanism exists to prevent.
	cloud := newFakeAllowAllCloud()
	groups := newAllowAllGroups(cloud)

	id, err := groups.id(context.Background(), "project-1", kuryrv1.SGDirectionIngress)
	if err != nil {
		t.Fatalf("id: %v", err)
	}

	got := cloud.ruleSet(id)
	want := map[string]bool{"ingress/IPv4": true, "ingress/IPv6": true}
	if len(got) != len(want) {
		t.Fatalf("rules = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("rules = %v, want %v", got, want)
		}
	}
}

func TestAllowAllEgressGroupOpensOnlyEgress(t *testing.T) {
	cloud := newFakeAllowAllCloud()
	groups := newAllowAllGroups(cloud)

	id, err := groups.id(context.Background(), "project-1", kuryrv1.SGDirectionEgress)
	if err != nil {
		t.Fatalf("id: %v", err)
	}

	got := cloud.ruleSet(id)
	if got["ingress/IPv4"] || got["ingress/IPv6"] {
		t.Fatalf("rules = %v, want no ingress rules", got)
	}
	if !got["egress/IPv4"] || !got["egress/IPv6"] {
		t.Fatalf("rules = %v, want both egress allow-alls", got)
	}
}

func TestAllowAllGroupIsCreatedOncePerProject(t *testing.T) {
	cloud := newFakeAllowAllCloud()
	groups := newAllowAllGroups(cloud)
	ctx := context.Background()

	first, err := groups.id(ctx, "project-1", kuryrv1.SGDirectionIngress)
	if err != nil {
		t.Fatalf("id: %v", err)
	}
	second, err := groups.id(ctx, "project-1", kuryrv1.SGDirectionIngress)
	if err != nil {
		t.Fatalf("id: %v", err)
	}
	if first != second {
		t.Fatalf("got two different groups, %q and %q", first, second)
	}
	if cloud.creates != 1 {
		t.Errorf("created %d groups, want 1", cloud.creates)
	}

	// A different project needs its own group: a pod's groups have to live in
	// the same project as its port.
	other, err := groups.id(ctx, "project-2", kuryrv1.SGDirectionIngress)
	if err != nil {
		t.Fatalf("id: %v", err)
	}
	if other == first {
		t.Error("project-2 reused project-1's security group")
	}
	if cloud.creates != 2 {
		t.Errorf("created %d groups, want 2", cloud.creates)
	}
}

func TestAllowAllGroupAdoptsAndRepairsAnExistingGroup(t *testing.T) {
	// A group created by an earlier run that died before adding its rules
	// must be finished rather than left as a silent hole.
	cloud := newFakeAllowAllCloud()
	cloud.groups = append(cloud.groups, &openstack.SecurityGroup{
		ID: "sg-existing", Name: allowAllIngressGroupName, ProjectID: "project-1",
	})
	groups := newAllowAllGroups(cloud)

	id, err := groups.id(context.Background(), "project-1", kuryrv1.SGDirectionIngress)
	if err != nil {
		t.Fatalf("id: %v", err)
	}
	if id != "sg-existing" {
		t.Fatalf("group = %q, want the existing sg-existing", id)
	}
	if cloud.creates != 0 {
		t.Errorf("created %d groups, want none: the existing one should be adopted", cloud.creates)
	}
	got := cloud.ruleSet(id)
	if !got["ingress/IPv4"] || !got["ingress/IPv6"] {
		t.Errorf("rules = %v, want the missing allow-alls to have been added", got)
	}
}

func TestAllowAllGroupRefusesDuplicates(t *testing.T) {
	// Picking one arbitrarily would attach it to some ports and not others.
	cloud := newFakeAllowAllCloud()
	for _, id := range []string{"sg-a", "sg-b"} {
		cloud.groups = append(cloud.groups, &openstack.SecurityGroup{
			ID: id, Name: allowAllIngressGroupName, ProjectID: "project-1",
		})
	}
	groups := newAllowAllGroups(cloud)

	if _, err := groups.id(context.Background(), "project-1", kuryrv1.SGDirectionIngress); err == nil {
		t.Fatal("expected an error when two groups share the name")
	}
}
