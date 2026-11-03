package openstack

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/attributestags"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/portsbinding"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// Neutron port states and the device_owner Kuryr stamps on its ports.
const (
	PortStatusActive = "ACTIVE"
	PortStatusDown   = "DOWN"

	// DeviceOwner marks a port as belonging to a Kuryr-managed pod. It
	// doubles as the filter used to find leftovers during garbage collection.
	DeviceOwner = "compute:kuryr"

	// DeviceOwnerSubport is what Neutron sets on a port attached to a trunk.
	DeviceOwnerSubport = "trunk:subport"

	// PoolPortName is the name given to a port sitting unused in a pool. The
	// recovery path after a controller restart finds pooled ports by it, so
	// it matches the name the Python implementation used and a mixed or
	// migrating deployment recognises its own ports either way.
	PoolPortName = "kuryr-pool-port"

	// VIFTypeBindingFailed is what Neutron reports when it could not bind.
	VIFTypeBindingFailed = "binding_failed"
	// VIFTypeUnbound is reported for a port not yet bound to a host.
	VIFTypeUnbound = "unbound"
)

// ErrBindingFailed is returned when Neutron could not bind a port. It is not
// retryable by itself; something in the Neutron deployment is wrong.
var ErrBindingFailed = errors.New("neutron reported binding_failed for the port")

// ErrRevisionConflict means Neutron rejected an update because the port had
// changed since it was read. The caller re-reads and tries again; it is the
// expected outcome when several pods on one node update the same parent port
// at the same time.
var ErrRevisionConflict = errors.New("the port was modified concurrently")

// FixedIP is one address allocation on a port.
type FixedIP struct {
	SubnetID  string `json:"subnet_id,omitempty"`
	IPAddress string `json:"ip_address,omitempty"`
}

// AddressPair is one entry of a port's allowed_address_pairs.
//
// Neutron drops any packet whose source address is not one the port owns.
// A macvlan child inside a pod sends from its own address over the node VM's
// port, so without an entry here every packet it sends is discarded.
type AddressPair struct {
	IPAddress  string `json:"ip_address"`
	MACAddress string `json:"mac_address,omitempty"`
}

// PortSpec is a request to create a port.
type PortSpec struct {
	NetworkID      string
	Name           string
	ProjectID      string
	DeviceID       string
	DeviceOwner    string
	HostID         string
	FixedIPs       []FixedIP
	SecurityGroups []string
	Tags           []string
}

// Port is a Neutron port as Kuryr needs to see it: the base attributes plus
// the binding extension, which is where the VIF type and bridge name live.
type Port struct {
	ID             string
	NetworkID      string
	Name           string
	Status         string
	MACAddress     string
	DeviceID       string
	DeviceOwner    string
	ProjectID      string
	HostID         string
	FixedIPs       []FixedIP
	SecurityGroups []string
	Tags           []string

	// AllowedAddressPairs are the addresses this port may also send from.
	AllowedAddressPairs []AddressPair
	// RevisionNumber is Neutron's optimistic concurrency token. An update
	// that carries it is rejected if anything else changed the port first,
	// which is what makes read-modify-write on a shared port safe.
	RevisionNumber int

	VIFType    string
	VIFDetails map[string]any
}

// IsActive reports whether Neutron has finished programming the dataplane.
func (p *Port) IsActive() bool { return p.Status == PortStatusActive }

// BoolFromDetails reads a boolean out of binding:vif_details, which Neutron
// types loosely.
func (p *Port) BoolFromDetails(key string) bool {
	v, ok := p.VIFDetails[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// StringFromDetails reads a string out of binding:vif_details.
func (p *Port) StringFromDetails(key string) string {
	v, ok := p.VIFDetails[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// portWithBinding is the wire shape of a port including the binding extension.
type portWithBinding struct {
	ports.Port
	portsbinding.PortsBindingExt
}

func (p *portWithBinding) toPort() *Port {
	out := &Port{
		ID:             p.ID,
		NetworkID:      p.NetworkID,
		Name:           p.Name,
		Status:         p.Status,
		MACAddress:     p.MACAddress,
		DeviceID:       p.DeviceID,
		DeviceOwner:    p.DeviceOwner,
		ProjectID:      p.ProjectID,
		HostID:         p.HostID,
		SecurityGroups: p.SecurityGroups,
		Tags:           p.Tags,
		RevisionNumber: p.RevisionNumber,
		VIFType:        p.VIFType,
		VIFDetails:     p.VIFDetails,
	}
	for _, ip := range p.FixedIPs {
		out.FixedIPs = append(out.FixedIPs, FixedIP{SubnetID: ip.SubnetID, IPAddress: ip.IPAddress})
	}
	for _, pair := range p.AllowedAddressPairs {
		out.AllowedAddressPairs = append(out.AllowedAddressPairs,
			AddressPair{IPAddress: pair.IPAddress, MACAddress: pair.MACAddress})
	}
	return out
}

// createBody renders a PortSpec into the Neutron request body, folding in the
// binding extension and, where supported, the tags.
func (c *Client) createBody(spec PortSpec) map[string]any {
	body := map[string]any{
		"network_id":     spec.NetworkID,
		"admin_state_up": true,
	}
	if spec.Name != "" {
		body["name"] = spec.Name
	}
	if spec.ProjectID != "" {
		body["project_id"] = spec.ProjectID
	}
	if spec.DeviceID != "" {
		body["device_id"] = spec.DeviceID
	}
	if spec.DeviceOwner != "" {
		body["device_owner"] = spec.DeviceOwner
	}
	if spec.HostID != "" {
		body["binding:host_id"] = spec.HostID
	}
	if len(spec.SecurityGroups) > 0 {
		body["security_groups"] = spec.SecurityGroups
	}
	if len(spec.FixedIPs) > 0 {
		fixed := make([]map[string]string, 0, len(spec.FixedIPs))
		for _, ip := range spec.FixedIPs {
			e := map[string]string{}
			if ip.SubnetID != "" {
				e["subnet_id"] = ip.SubnetID
			}
			if ip.IPAddress != "" {
				e["ip_address"] = ip.IPAddress
			}
			fixed = append(fixed, e)
		}
		body["fixed_ips"] = fixed
	}
	tags := spec.Tags
	if len(tags) == 0 {
		tags = c.Tags
	}
	if len(tags) > 0 && c.caps.TagOnCreate {
		body["tags"] = tags
	}
	return body
}

// CreatePort creates a single port.
func (c *Client) CreatePort(ctx context.Context, spec PortSpec) (*Port, error) {
	var resp struct {
		Port portWithBinding `json:"port"`
	}
	body := map[string]any{"port": c.createBody(spec)}

	if _, err := c.Network.Post(ctx, c.Network.ServiceURL("ports"), body, &resp,
		&gophercloud.RequestOpts{OkCodes: []int{201}}); err != nil {
		return nil, fmt.Errorf("creating port on network %s: %w", spec.NetworkID, err)
	}

	port := resp.Port.toPort()
	if err := c.finishPorts(ctx, []*Port{port}, spec.Tags); err != nil {
		return nil, err
	}
	return port, nil
}

// CreatePortsBulk creates many identical ports in a single Neutron call.
//
// gophercloud has no helper for this, but the pool driver depends on it: the
// whole point of the pool is to amortise Neutron round trips, and creating
// ports one at a time defeats that. Neutron accepts a list under the "ports"
// key and either creates all of them or none.
func (c *Client) CreatePortsBulk(ctx context.Context, spec PortSpec, count int) ([]*Port, error) {
	if count <= 0 {
		return nil, nil
	}

	bodies := make([]map[string]any, count)
	for i := range bodies {
		bodies[i] = c.createBody(spec)
	}

	var resp struct {
		Ports []portWithBinding `json:"ports"`
	}
	if _, err := c.Network.Post(ctx, c.Network.ServiceURL("ports"),
		map[string]any{"ports": bodies}, &resp,
		&gophercloud.RequestOpts{OkCodes: []int{201}}); err != nil {
		return nil, fmt.Errorf("bulk-creating %d ports on network %s: %w", count, spec.NetworkID, err)
	}

	out := make([]*Port, 0, len(resp.Ports))
	for i := range resp.Ports {
		out = append(out, resp.Ports[i].toPort())
	}
	if err := c.finishPorts(ctx, out, spec.Tags); err != nil {
		return nil, err
	}
	return out, nil
}

// finishPorts applies the two post-create steps: reject a failed binding, and
// tag the ports when Neutron could not do it inline.
//
// A binding failure has to delete the ports it just made. Leaving them behind
// would burn quota on every retry, which is how a transient Neutron problem
// turns into an exhausted project.
func (c *Client) finishPorts(ctx context.Context, created []*Port, tags []string) error {
	if len(created) == 0 {
		return nil
	}

	if created[0].VIFType == VIFTypeBindingFailed {
		ids := make([]string, 0, len(created))
		for _, p := range created {
			ids = append(ids, p.ID)
		}
		if err := c.DeletePorts(ctx, ids); err != nil {
			return fmt.Errorf("%w (network %s; deleting the failed ports also failed: %w)",
				ErrBindingFailed, created[0].NetworkID, err)
		}
		return fmt.Errorf("%w (network %s)", ErrBindingFailed, created[0].NetworkID)
	}

	if len(tags) == 0 {
		tags = c.Tags
	}
	if len(tags) == 0 || c.caps.TagOnCreate {
		return nil
	}
	for _, p := range created {
		if err := c.TagPort(ctx, p.ID, tags); err != nil {
			return err
		}
		p.Tags = tags
	}
	return nil
}

// GetPort fetches one port.
func (c *Client) GetPort(ctx context.Context, id string) (*Port, error) {
	var resp struct {
		Port portWithBinding `json:"port"`
	}
	if _, err := c.Network.Get(ctx, c.Network.ServiceURL("ports", id), &resp, nil); err != nil {
		return nil, fmt.Errorf("getting port %s: %w", id, err)
	}
	return resp.Port.toPort(), nil
}

// PortFilter narrows a port listing.
type PortFilter struct {
	NetworkID   string
	DeviceID    string
	DeviceOwner string
	Name        string
	Status      string
	HostID      string
	ProjectID   string
	Tags        []string
}

// ListPorts returns every port matching the filter, following pagination.
func (c *Client) ListPorts(ctx context.Context, f PortFilter) ([]*Port, error) {
	opts := ports.ListOpts{
		NetworkID:   f.NetworkID,
		DeviceID:    f.DeviceID,
		DeviceOwner: f.DeviceOwner,
		Name:        f.Name,
		Status:      f.Status,
		ProjectID:   f.ProjectID,
	}
	if len(f.Tags) > 0 {
		opts.Tags = joinTags(f.Tags)
	}

	var out []*Port
	err := ports.List(c.Network, opts).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		var list []portWithBinding
		if err := ports.ExtractPortsInto(page, &list); err != nil {
			return false, err
		}
		for i := range list {
			if f.HostID != "" && list[i].HostID != f.HostID {
				continue
			}
			out = append(out, list[i].toPort())
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing ports: %w", err)
	}
	return out, nil
}

// PortUpdate carries the mutable attributes of a port. A nil field is left
// alone, which matters because clearing security groups by accident would
// silently open a pod up.
type PortUpdate struct {
	Name           *string
	DeviceID       *string
	DeviceOwner    *string
	HostID         *string
	SecurityGroups *[]string
	// AllowedAddressPairs replaces the whole list. Neutron has no way to add
	// or remove a single pair, so every change is a read-modify-write, which
	// is why IfRevision exists.
	AllowedAddressPairs *[]AddressPair
	// IfRevision makes the update conditional on the port still being at that
	// revision. Neutron answers 412 otherwise, which UpdatePort reports as
	// ErrRevisionConflict.
	IfRevision *int
}

// UpdatePort applies a partial update.
func (c *Client) UpdatePort(ctx context.Context, id string, u PortUpdate) (*Port, error) {
	body := map[string]any{}
	if u.Name != nil {
		body["name"] = *u.Name
	}
	if u.DeviceID != nil {
		body["device_id"] = *u.DeviceID
	}
	if u.DeviceOwner != nil {
		body["device_owner"] = *u.DeviceOwner
	}
	if u.HostID != nil {
		body["binding:host_id"] = *u.HostID
	}
	if u.SecurityGroups != nil {
		body["security_groups"] = *u.SecurityGroups
	}
	if u.AllowedAddressPairs != nil {
		// Encode an empty list as [], not null: null leaves the pairs alone
		// while [] clears them, and clearing is what removing the last pair
		// has to do.
		pairs := *u.AllowedAddressPairs
		if pairs == nil {
			pairs = []AddressPair{}
		}
		body["allowed_address_pairs"] = pairs
	}
	if len(body) == 0 {
		return c.GetPort(ctx, id)
	}

	opts := &gophercloud.RequestOpts{OkCodes: []int{200}}
	if u.IfRevision != nil {
		opts.MoreHeaders = map[string]string{
			"If-Match": fmt.Sprintf("revision_number=%d", *u.IfRevision),
		}
	}

	var resp struct {
		Port portWithBinding `json:"port"`
	}
	if _, err := c.Network.Put(ctx, c.Network.ServiceURL("ports", id),
		map[string]any{"port": body}, &resp, opts); err != nil {
		if IsRevisionConflict(err) {
			return nil, fmt.Errorf("updating port %s: %w", id, ErrRevisionConflict)
		}
		return nil, fmt.Errorf("updating port %s: %w", id, err)
	}
	return resp.Port.toPort(), nil
}

// addressPairRetries bounds how many times a read-modify-write on a shared
// port is retried. Every pod starting on one node updates the same parent
// port, so losing the race is routine; losing it this many times in a row is
// not, and means something else is rewriting the port continuously.
const addressPairRetries = 5

// UpdateAllowedAddressPairs re-reads a port, applies change to its address
// pairs, and writes the result back only if nothing else touched the port in
// between.
//
// It exists because Neutron replaces the whole list on update. Two pods
// starting on one node would otherwise each read the list, add their own
// address, and write back — and the second write would silently drop the
// first pod's entry, cutting its traffic off with nothing in any log to say
// why.
func (c *Client) UpdateAllowedAddressPairs(ctx context.Context, portID string,
	change func([]AddressPair) []AddressPair) error {

	var lastErr error
	for range addressPairRetries {
		port, err := c.GetPort(ctx, portID)
		if err != nil {
			return err
		}

		updated := change(append([]AddressPair(nil), port.AllowedAddressPairs...))
		if equalAddressPairs(port.AllowedAddressPairs, updated) {
			return nil
		}

		_, err = c.UpdatePort(ctx, portID, PortUpdate{
			AllowedAddressPairs: &updated,
			IfRevision:          &port.RevisionNumber,
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrRevisionConflict) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("updating the allowed address pairs of port %s: %w", portID, lastErr)
}

// equalAddressPairs reports whether two pair lists are the same, so an update
// that would change nothing is skipped rather than sent.
func equalAddressPairs(a, b []AddressPair) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// IsRevisionConflict reports whether Neutron rejected a conditional update
// because the resource had moved on.
func IsRevisionConflict(err error) bool {
	return gophercloud.ResponseCodeIs(err, 412)
}

// DeletePort removes a port. A port that is already gone is not an error;
// deletion is the one operation that must be idempotent everywhere, because
// every cleanup path can be retried.
func (c *Client) DeletePort(ctx context.Context, id string) error {
	err := ports.Delete(ctx, c.Network, id).ExtractErr()
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("deleting port %s: %w", id, err)
	}
	return nil
}

// DeletePorts removes many ports concurrently and reports the combined error.
func (c *Client) DeletePorts(ctx context.Context, ids []string) error {
	const maxParallel = 10

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		sem  = make(chan struct{}, maxParallel)
	)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := c.DeletePort(ctx, id); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// TagPort replaces the tags on a port.
func (c *Client) TagPort(ctx context.Context, id string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	err := attributestags.ReplaceAll(ctx, c.Network, "ports", id,
		attributestags.ReplaceAllOpts{Tags: tags}).Err
	if err != nil {
		return fmt.Errorf("tagging port %s: %w", id, err)
	}
	return nil
}

func joinTags(tags []string) string { return strings.Join(tags, ",") }

// IsNotFound reports whether an OpenStack error is a 404.
func IsNotFound(err error) bool {
	var e gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &e) {
		return e.Actual == 404
	}
	return gophercloud.ResponseCodeIs(err, 404)
}

// IsConflict reports whether an OpenStack error is a 409, which Neutron uses
// both for genuine conflicts and for quota exhaustion.
func IsConflict(err error) bool {
	return gophercloud.ResponseCodeIs(err, 409)
}

// IsOverQuota reports whether a conflict was specifically a quota problem.
// Quota is worth retrying, since pods get deleted and free ports up; other
// conflicts are not.
func IsOverQuota(err error) bool {
	if !IsConflict(err) {
		return false
	}
	var e gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &e) {
		// Neutron has no machine-readable marker for this, so the body is
		// all there is to go on, and its wording differs by release.
		body := strings.ToLower(string(e.Body))
		return strings.Contains(body, "overquota") || strings.Contains(body, "quota exceeded")
	}
	return false
}

// FindPortByFixedIP locates the node VM port holding a given address. This is
// how a nested VIF driver turns a pod host IP into the node VM parent port,
// and from there into the trunk pod subports attach to.
//
// As in Python, the port must hold the address on one of the worker node
// subnets and carry the resource tags. An address alone is not unique: with
// overlapping CIDRs another network can hold the same IP, and taking the
// first match would attach pods to some other VM.
func (c *Client) FindPortByFixedIP(ctx context.Context, ip string, nodeSubnets []string) (*Port, error) {
	if len(nodeSubnets) == 0 {
		return nil, fmt.Errorf("[pod_vif_nested]worker_nodes_subnets is empty, so the node VM port "+
			"holding %s cannot be told apart from other ports with that address", ip)
	}
	opts := ports.ListOpts{
		FixedIPs: []ports.FixedIPOpts{{IPAddress: ip}},
		Tags:     strings.Join(c.Tags, ","),
	}

	var found *Port
	err := ports.List(c.Network, opts).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		var list []portWithBinding
		if err := ports.ExtractPortsInto(page, &list); err != nil {
			return false, err
		}
		for i := range list {
			port := list[i].toPort()
			if onSubnets(port, ip, nodeSubnets) {
				found = port
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("looking up port by fixed IP %s: %w", ip, err)
	}
	if found == nil {
		return nil, fmt.Errorf("no Neutron port holds address %s on the worker node subnets %v", ip, nodeSubnets)
	}
	return found, nil
}

// onSubnets reports whether port holds ip on one of subnets.
func onSubnets(port *Port, ip string, subnets []string) bool {
	for _, fip := range port.FixedIPs {
		if fip.IPAddress == ip && slices.Contains(subnets, fip.SubnetID) {
			return true
		}
	}
	return false
}
