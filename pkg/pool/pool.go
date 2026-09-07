// Package pool keeps pre-created Neutron ports ready so that starting a pod
// does not have to wait for one.
//
// This replaces kuryr_kubernetes/controller/drivers/vif_pool.py, the largest
// module in the Python tree at around 1300 lines. Most of that size came from
// three near-duplicate pool classes, one per VIF driver, each reimplementing
// port deletion, security group updates and recycling.
//
// Here the pool is a decorator around a drivers.PodVIFDriver rather than a
// parallel hierarchy beside it. Creating and destroying ports is delegated to
// the wrapped driver, which already knows how to detach a trunk subport or
// release a VLAN id, so none of that is duplicated. What is left is the pool
// itself: bookkeeping, a recycler and a repopulator.
//
// That also fixes the REVISIT comment the Python carried for years, where the
// pod handler had to know about the pool driver directly. Nothing outside
// this package knows a pool exists.
package pool

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

// ErrNotReady means the pool has not finished recovering pre-created ports
// from a previous run. Serving pods before then would create duplicates of
// ports that already exist.
var ErrNotReady = fmt.Errorf("ports pool has not finished recovering yet: %w", drivers.ErrRetryLater)

// ErrPoolEmpty means no port was available and repopulation did not produce
// one in time. It is retryable: the repopulator is already working on it.
var ErrPoolEmpty = fmt.Errorf("no port available in the pool: %w", drivers.ErrRetryLater)

// populateTimeout bounds how long one caller waits for the right to
// repopulate a pool. Only one goroutine populates a given pool at a time; the
// rest would only pile duplicate bulk creates onto Neutron.
const populateTimeout = 7 * time.Minute

// key identifies one pool.
//
// Security groups are deliberately absent. A NetworkPolicy can change the
// groups on a port that is already in use, so keying on them would shatter
// the pool into fragments that never match anything. Groups are handled one
// level down instead, and a port can be moved between them with a single
// update.
//
// Subnets are part of the key, and the network is not enough on its own. Two
// subnets can share a network, and a pod that named one of them through the
// subnet annotation must not be handed a port holding an address on the
// other: the address would be routable but outside the range the tenant
// asked for.
type key struct {
	Host      string
	ProjectID string
	NetworkID string
	// Subnets is the canonical form of the subnet set the ports hold
	// addresses on: sorted IDs joined by commas.
	Subnets string
}

func (k key) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", k.Host, k.ProjectID, k.NetworkID, k.Subnets)
}

// makeSubnetKey is the canonical form of a subnet set, so that the same
// subnets always name the same pool whatever order they arrive in.
func makeSubnetKey(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return strings.Join(slices.Sorted(slices.Values(ids)), ",")
}

// subnetKeyOf is makeSubnetKey for the subnets a driver resolved.
func subnetKeyOf(subnets vif.SubnetSet) string {
	return makeSubnetKey(slices.Collect(maps.Keys(subnets)))
}

// subnetKeyOfVIF is makeSubnetKey for a port that already exists. The VIF
// records the subnets its addresses came from, which is what lets a released
// or recovered port find its pool without another Neutron call.
func subnetKeyOfVIF(v *kuryrv1.VIF) string {
	ids := make([]string, 0, len(v.Network.Subnets))
	for _, sub := range v.Network.Subnets {
		if sub.ID != "" {
			ids = append(ids, sub.ID)
		}
	}
	return makeSubnetKey(ids)
}

// sgKey is a canonical form of a security group set.
type sgKey string

func makeSGKey(sgs []string) sgKey {
	if len(sgs) == 0 {
		return ""
	}
	return sgKey(strings.Join(slices.Sorted(slices.Values(sgs)), ","))
}

// bucket holds the free ports of one pool, grouped by security group set.
//
// order is a least-recently-refreshed list of groups. When a pod needs groups
// no free port carries, the port is taken from the front, which is the group
// that has gone longest without being touched. That keeps a rarely used group
// from hoarding ports.
type bucket struct {
	groups map[sgKey][]string
	order  []sgKey
}

func newBucket() *bucket {
	return &bucket{groups: map[sgKey][]string{}}
}

func (b *bucket) size() int {
	n := 0
	for _, ports := range b.groups {
		n += len(ports)
	}
	return n
}

// touch moves a group to the back of the order, marking it freshest.
func (b *bucket) touch(sg sgKey) {
	for i, existing := range b.order {
		if existing == sg {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
	b.order = append(b.order, sg)
}

func (b *bucket) push(sg sgKey, portID string) {
	b.groups[sg] = append(b.groups[sg], portID)
	b.touch(sg)
}

// pop takes a port, preferring an exact security group match. The second
// return value reports whether the port needs its groups rewritten.
func (b *bucket) pop(want sgKey) (string, bool, bool) {
	if ports := b.groups[want]; len(ports) > 0 {
		portID := ports[len(ports)-1]
		b.groups[want] = ports[:len(ports)-1]
		return portID, false, true
	}

	for _, sg := range b.order {
		ports := b.groups[sg]
		if len(ports) == 0 {
			continue
		}
		portID := ports[len(ports)-1]
		b.groups[sg] = ports[:len(ports)-1]
		return portID, true, true
	}
	return "", false, false
}

// remove drops a specific port, used when it turns out to be unusable.
func (b *bucket) remove(portID string) bool {
	for sg, ports := range b.groups {
		for i, id := range ports {
			if id == portID {
				b.groups[sg] = append(ports[:i], ports[i+1:]...)
				return true
			}
		}
	}
	return false
}

// Pool is a drivers.PodVIFDriver that serves ports from a warm pool.
type Pool struct {
	inner    drivers.PodVIFDriver
	strategy strategy
	os       cloud
	k8s      client.Client
	cfg      *config.Config
	metrics  *Metrics

	mu         sync.Mutex
	available  map[key]*bucket
	vifs       map[string]*kuryrv1.VIF // port id -> VIF, for ports we own
	recyclable map[string]key          // port id -> pool it belongs to

	// populateLocks serialises repopulation per pool.
	populateLocks sync.Map // key -> chan struct{}

	ready atomic.Bool
}

var _ drivers.PodVIFDriver = (*Pool)(nil)

// New wraps a VIF driver in a pool.
//
// The strategy is chosen from the wrapped driver: a baremetal pool keys on
// node name and recovers by listing ports, a nested one keys on the node VM
// address and recovers by walking trunks.
func New(cfg *config.Config, os cloud, k8s client.Client,
	inner drivers.PodVIFDriver, metrics *Metrics) (*Pool, error) {

	strategy, err := strategyFor(inner.Name(), os, k8s, cfg)
	if err != nil {
		return nil, err
	}

	return &Pool{
		inner:      inner,
		strategy:   strategy,
		os:         os,
		k8s:        k8s,
		cfg:        cfg,
		metrics:    metrics,
		available:  map[key]*bucket{},
		vifs:       map[string]*kuryrv1.VIF{},
		recyclable: map[string]key{},
	}, nil
}

// Name reports the wrapped driver, so logs and metrics still say what kind of
// port this is.
func (p *Pool) Name() string { return p.inner.Name() + "+pool" }

// Ready reports whether recovery has finished.
func (p *Pool) Ready() bool { return p.ready.Load() }

// RequestVIF serves a port from the pool, repopulating if it comes up empty.
func (p *Pool) RequestVIF(ctx context.Context, req drivers.PodVIFRequest) (*kuryrv1.VIF, error) {
	if !p.ready.Load() {
		return nil, ErrNotReady
	}

	k, err := p.keyFor(req)
	if err != nil {
		return nil, err
	}
	want := makeSGKey(req.SecurityGroups)

	if v, err := p.take(ctx, k, want, req); err != nil {
		return nil, err
	} else if v != nil {
		// Top the pool back up without making this pod wait for it.
		go p.populateAsync(k, req)
		return v, nil
	}

	// Nothing free. Fill the pool and try once more before giving up, so the
	// first pod on a node does not have to fail and be retried.
	if err := p.populate(ctx, k, req); err != nil {
		return nil, err
	}
	if v, err := p.take(ctx, k, want, req); err != nil {
		return nil, err
	} else if v != nil {
		return v, nil
	}
	return nil, fmt.Errorf("%w for %s", ErrPoolEmpty, k)
}

// take removes one port from a pool and prepares it for a pod. It returns a
// nil VIF, and no error, when the pool is empty.
func (p *Pool) take(ctx context.Context, k key, want sgKey, req drivers.PodVIFRequest) (*kuryrv1.VIF, error) {
	p.mu.Lock()
	b, ok := p.available[k]
	if !ok {
		p.mu.Unlock()
		return nil, nil
	}
	portID, needsSGUpdate, found := b.pop(want)
	if !found {
		p.mu.Unlock()
		return nil, nil
	}
	v, known := p.vifs[portID]
	p.mu.Unlock()

	if !known {
		// The port is in a pool but we have no VIF for it. That should not
		// happen; drop it rather than hand back something unusable.
		log.FromContext(ctx).Info("pooled port has no VIF recorded, discarding it", "portID", portID)
		return nil, nil
	}

	if needsSGUpdate {
		if err := p.inner.UpdateVIFSecurityGroups(ctx, v, req.SecurityGroups); err != nil {
			// Put it back: the port is still good, just not for this pod.
			p.mu.Lock()
			p.bucketFor(k).push(makeSGKey(nil), portID)
			p.mu.Unlock()
			return nil, fmt.Errorf("re-applying security groups to pooled port %s: %w", portID, err)
		}
	}

	if p.cfg.Kubernetes.PortDebug && req.Pod != nil {
		if err := p.strategy.MarkInUse(ctx, portID, req.Pod); err != nil {
			// Naming is a debugging aid; failing the pod over it would be a
			// poor trade.
			log.FromContext(ctx).V(1).Info("could not rename pooled port", "portID", portID, "error", err)
		}
	}

	p.metrics.observePoolSize(k, p.sizeOf(k))
	p.metrics.reuse(p.inner.Name())

	// Hand back a copy: the caller writes it into a CRD and may mutate it.
	out := v.DeepCopy()
	return out, nil
}

// populate fills a pool up to ports_pool_min, creating in bulk.
func (p *Pool) populate(ctx context.Context, k key, req drivers.PodVIFRequest) error {
	// One repopulation per pool key at a time; a second caller waits rather
	// than doubling the ports created.
	lockAny, _ := p.populateLocks.LoadOrStore(k, make(chan struct{}, 1))
	lock := lockAny.(chan struct{})
	select {
	case lock <- struct{}{}:
		defer func() { <-lock }()
	case <-time.After(populateTimeout):
		return fmt.Errorf("timed out waiting to repopulate pool %s", k)
	case <-ctx.Done():
		return ctx.Err()
	}

	size := p.sizeOf(k)
	minSize := p.cfg.VIFPool.PortsPoolMin
	if size >= minSize {
		return nil
	}

	want := minSize - size
	if batch := p.cfg.VIFPool.PortsPoolBatch; batch > want {
		want = batch
	}
	if maxSize := p.cfg.VIFPool.PortsPoolMax; maxSize > 0 && size+want > maxSize {
		want = maxSize - size
	}
	if want <= 0 {
		return nil
	}

	logger := log.FromContext(ctx)
	logger.V(1).Info("repopulating ports pool", "pool", k.String(), "have", size, "creating", want)

	bulkReq := req
	bulkReq.HostID = k.Host
	vifs, err := p.inner.RequestVIFs(ctx, bulkReq, want)
	if err != nil {
		p.metrics.populateFailure(p.inner.Name())
		return fmt.Errorf("repopulating pool %s: %w", k, err)
	}

	sg := makeSGKey(req.SecurityGroups)
	p.mu.Lock()
	b := p.bucketFor(k)
	for _, v := range vifs {
		p.vifs[v.ID] = v
		b.push(sg, v.ID)
	}
	p.mu.Unlock()

	p.metrics.created(p.inner.Name(), len(vifs))
	p.metrics.observePoolSize(k, p.sizeOf(k))
	logger.V(1).Info("ports pool repopulated", "pool", k.String(), "added", len(vifs))
	return nil
}

// populateAsync tops a pool up in the background, swallowing errors: the pod
// that triggered it already has its port.
func (p *Pool) populateAsync(k key, req drivers.PodVIFRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), populateTimeout)
	defer cancel()

	if err := p.populate(ctx, k, req); err != nil {
		log.Log.V(1).Info("background pool repopulation failed", "pool", k.String(), "error", err)
	}
}

// RequestVIFs is passed straight through. Bulk creation is what fills pools;
// it is never itself served from one.
func (p *Pool) RequestVIFs(ctx context.Context, req drivers.PodVIFRequest, count int) ([]*kuryrv1.VIF, error) {
	return p.inner.RequestVIFs(ctx, req, count)
}

// ReleaseVIF hands a port back to be recycled rather than deleting it.
//
// This is the other half of the saving: pod deletion makes no Neutron call at
// all on the hot path.
func (p *Pool) ReleaseVIF(ctx context.Context, v *kuryrv1.VIF, req drivers.PodVIFRequest) error {
	if !p.ready.Load() {
		return ErrNotReady
	}

	k, err := p.keyForRelease(v, req)
	if err != nil {
		// We cannot work out which pool it came from, so recycling is not an
		// option; delete it rather than leak it.
		log.FromContext(ctx).V(1).Info("cannot determine pool for released port, deleting it",
			"portID", v.ID, "reason", err)
		return p.inner.ReleaseVIF(ctx, v, req)
	}

	p.mu.Lock()
	if _, known := p.vifs[v.ID]; !known {
		p.vifs[v.ID] = v.DeepCopy()
	}
	p.recyclable[v.ID] = k
	p.mu.Unlock()

	p.metrics.recycled(p.inner.Name())
	return nil
}

// ActivateVIF and UpdateVIFSecurityGroups have nothing to do with pooling.
func (p *Pool) ActivateVIF(ctx context.Context, v *kuryrv1.VIF, req drivers.PodVIFRequest) error {
	return p.inner.ActivateVIF(ctx, v, req)
}

// UpdateVIFSecurityGroups passes straight through to the wrapped driver. A
// live port's groups are not something the pool tracks: it only decides which
// free port a pod gets, and by this point the pod already has one.
func (p *Pool) UpdateVIFSecurityGroups(ctx context.Context, v *kuryrv1.VIF, sgs []string) error {
	return p.inner.UpdateVIFSecurityGroups(ctx, v, sgs)
}

// Recycle moves released ports back into their pools, or deletes them when a
// pool is already at its maximum.
//
// Security groups are re-read from Neutron first. A port that was in use may
// have had its groups rewritten by a NetworkPolicy, and filing it under the
// groups it had when it was handed out would make the next pod inherit the
// wrong ones.
func (p *Pool) Recycle(ctx context.Context) error {
	p.mu.Lock()
	pending := make(map[string]key, len(p.recyclable))
	for portID, k := range p.recyclable {
		pending[portID] = k
	}
	p.mu.Unlock()

	if len(pending) == 0 {
		return nil
	}

	logger := log.FromContext(ctx)
	current := p.currentSecurityGroups(ctx, pending)

	maxSize := p.cfg.VIFPool.PortsPoolMax
	var errs []error

	for portID, k := range pending {
		if maxSize > 0 && p.sizeOf(k) >= maxSize {
			p.mu.Lock()
			v := p.vifs[portID]
			p.mu.Unlock()

			if v != nil {
				// Delegate: the wrapped driver detaches the subport and frees
				// the VLAN id for a nested VIF, and just deletes the port for
				// a baremetal one.
				if err := p.inner.ReleaseVIF(ctx, v, drivers.PodVIFRequest{HostID: k.Host}); err != nil {
					errs = append(errs, fmt.Errorf("deleting surplus pooled port %s: %w", portID, err))
					continue
				}
			}
			p.forget(portID)
			p.metrics.deleted(p.inner.Name())
			continue
		}

		if p.cfg.Kubernetes.PortDebug {
			if err := p.strategy.MarkAvailable(ctx, portID); err != nil {
				// Leave it in the recyclable set and try again next tick.
				logger.V(1).Info("could not rename port for reuse, will retry",
					"portID", portID, "error", err)
				continue
			}
		}

		sg, ok := current[portID]
		if !ok {
			// Neutron did not report this port. It may have been deleted out
			// from under us; drop it rather than pool something that is gone.
			logger.V(1).Info("released port not found in Neutron, dropping it", "portID", portID)
			p.forget(portID)
			continue
		}

		p.mu.Lock()
		p.bucketFor(k).push(sg, portID)
		delete(p.recyclable, portID)
		p.mu.Unlock()

		p.metrics.observePoolSize(k, p.sizeOf(k))
	}

	return errors.Join(errs...)
}

// currentSecurityGroups reads back the groups on the ports being recycled.
func (p *Pool) currentSecurityGroups(ctx context.Context, pending map[string]key) map[string]sgKey {
	out := make(map[string]sgKey, len(pending))

	filter := openstack.PortFilter{DeviceOwner: openstack.DeviceOwner, Tags: p.os.ResourceTags()}
	ports, err := p.os.ListPorts(ctx, filter)
	if err != nil {
		log.FromContext(ctx).Error(err, "could not read back security groups for recycled ports")
		return out
	}
	for _, port := range ports {
		if _, wanted := pending[port.ID]; wanted {
			out[port.ID] = makeSGKey(port.SecurityGroups)
		}
	}

	// Subports carry a different device_owner, so a nested deployment needs a
	// second pass.
	if p.strategy.UsesTrunks() {
		subports, err := p.os.ListPorts(ctx,
			openstack.PortFilter{DeviceOwner: openstack.DeviceOwnerSubport, Tags: p.os.ResourceTags()})
		if err != nil {
			log.FromContext(ctx).Error(err, "could not read back security groups for recycled subports")
			return out
		}
		for _, port := range subports {
			if _, wanted := pending[port.ID]; wanted {
				out[port.ID] = makeSGKey(port.SecurityGroups)
			}
		}
	}
	return out
}

// RunRecycler drives the recycle loop until the context is cancelled.
func (p *Pool) RunRecycler(ctx context.Context) error {
	ticker := time.NewTicker(p.cfg.VIFPool.PortsPoolUpdateFrequency)
	defer ticker.Stop()

	logger := log.FromContext(ctx).WithName("pool-recycler")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !p.ready.Load() {
				continue
			}
			if err := p.Recycle(ctx); err != nil {
				// Never fatal: the ports stay queued and the next tick tries
				// again.
				logger.Error(err, "error while recycling ports, will retry",
					"interval", p.cfg.VIFPool.PortsPoolUpdateFrequency)
			}
		}
	}
}

// NeedLeaderElection reports true: only the active controller should be
// creating ports.
func (p *Pool) NeedLeaderElection() bool { return true }

// DeleteNetworkPools drops every pool on a network and deletes its ports.
// Namespace teardown calls this before removing the Neutron network, which
// would otherwise refuse to go while ports remain on it.
func (p *Pool) DeleteNetworkPools(ctx context.Context, networkID string) error {
	p.mu.Lock()
	var (
		doomed   []string
		affected []key
	)
	for k, b := range p.available {
		if k.NetworkID != networkID {
			continue
		}
		affected = append(affected, k)
		for _, ports := range b.groups {
			doomed = append(doomed, ports...)
		}
	}
	for _, k := range affected {
		delete(p.available, k)
	}
	vifsToDelete := make([]*kuryrv1.VIF, 0, len(doomed))
	for _, portID := range doomed {
		if v := p.vifs[portID]; v != nil {
			vifsToDelete = append(vifsToDelete, v)
		}
		delete(p.vifs, portID)
		delete(p.recyclable, portID)
	}
	p.mu.Unlock()

	var errs []error
	for _, v := range vifsToDelete {
		if err := p.inner.ReleaseVIF(ctx, v, drivers.PodVIFRequest{}); err != nil {
			errs = append(errs, err)
		}
	}
	if len(vifsToDelete) > 0 {
		log.FromContext(ctx).Info("released pooled ports for a removed network",
			"networkID", networkID, "ports", len(vifsToDelete))
	}
	return errors.Join(errs...)
}

// --- small helpers -----------------------------------------------------

// bucketFor must be called with the lock held.
func (p *Pool) bucketFor(k key) *bucket {
	b, ok := p.available[k]
	if !ok {
		b = newBucket()
		p.available[k] = b
	}
	return b
}

func (p *Pool) sizeOf(k key) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b, ok := p.available[k]; ok {
		return b.size()
	}
	return 0
}

func (p *Pool) forget(portID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.recyclable, portID)
	delete(p.vifs, portID)
	for _, b := range p.available {
		if b.remove(portID) {
			break
		}
	}
}

func (p *Pool) keyFor(req drivers.PodVIFRequest) (key, error) {
	host, err := p.strategy.HostKey(req)
	if err != nil {
		return key{}, err
	}
	netID, ok := req.Subnets.NetworkID()
	if !ok {
		return key{}, fmt.Errorf("pool: subnets for the pod span more than one network")
	}
	return key{
		Host:      host,
		ProjectID: req.ProjectID,
		NetworkID: netID,
		Subnets:   subnetKeyOf(req.Subnets),
	}, nil
}

func (p *Pool) keyForRelease(v *kuryrv1.VIF, req drivers.PodVIFRequest) (key, error) {
	host, err := p.strategy.HostKey(req)
	if err != nil {
		return key{}, err
	}
	return key{
		Host:      host,
		ProjectID: req.ProjectID,
		NetworkID: v.Network.ID,
		Subnets:   subnetKeyOfVIF(v),
	}, nil
}

// Stats reports pool occupancy, for the status command and tests.
func (p *Pool) Stats() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make(map[string]int, len(p.available))
	for k, b := range p.available {
		out[k.String()] = b.size()
	}
	return out
}

// podFor is a convenience for strategies that need the pod.
func podFor(req drivers.PodVIFRequest) *corev1.Pod { return req.Pod }

// Populate primes the pool for one node and network before any pod asks.
//
// Without it the first pod scheduled onto a node in a new namespace pays for a
// synchronous bulk create; with it that cost is moved to namespace creation,
// where nothing is waiting on it. Doing nothing when the pool is already at
// ports_pool_min makes this safe to call again.
func (p *Pool) Populate(ctx context.Context, node *corev1.Node, req drivers.PodVIFRequest) error {
	host, err := p.strategy.HostKeyForNode(node)
	if err != nil {
		return err
	}
	netID, ok := req.Subnets.NetworkID()
	if !ok {
		return fmt.Errorf("pool: subnets to prime span more than one network")
	}

	req.HostID = host
	return p.populate(ctx, key{
		Host:      host,
		ProjectID: req.ProjectID,
		NetworkID: netID,
		Subnets:   subnetKeyOf(req.Subnets),
	}, req)
}
