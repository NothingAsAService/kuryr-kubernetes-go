package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

const (
	testNetID  = "net-1"
	testSubID  = "subnet-1"
	testProj   = "project-1"
	testNode   = "worker-1"
	testHostIP = "10.0.0.5"
)

// --- fake inner VIF driver ---------------------------------------------

type fakeVIFDriver struct {
	mu sync.Mutex

	name       string
	next       int
	created    []string
	released   []string
	sgUpdates  map[string][]string
	requestErr error
}

func newFakeDriver(name string) *fakeVIFDriver {
	return &fakeVIFDriver{name: name, sgUpdates: map[string][]string{}}
}

func (f *fakeVIFDriver) Name() string { return f.name }

func (f *fakeVIFDriver) makeVIF() *kuryrv1.VIF {
	f.next++
	id := fmt.Sprintf("port-%d", f.next)
	f.created = append(f.created, id)
	return &kuryrv1.VIF{
		Type:    kuryrv1.VIFTypeOVS,
		ID:      id,
		Address: fmt.Sprintf("fa:16:3e:00:00:%02x", f.next),
		Bridge:  "br-int",
		Active:  true,
		Network: kuryrv1.Network{
			ID:      testNetID,
			Subnets: []kuryrv1.Subnet{{ID: testSubID, CIDR: "10.1.0.0/24", IPs: []string{"10.1.0.5"}}},
		},
	}
}

func (f *fakeVIFDriver) RequestVIF(_ context.Context, req drivers.PodVIFRequest) (*kuryrv1.VIF, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requestErr != nil {
		return nil, f.requestErr
	}
	return f.makeVIF(), nil
}

func (f *fakeVIFDriver) RequestVIFs(_ context.Context, req drivers.PodVIFRequest, n int) ([]*kuryrv1.VIF, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requestErr != nil {
		return nil, f.requestErr
	}
	out := make([]*kuryrv1.VIF, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, f.makeVIF())
	}
	return out, nil
}

func (f *fakeVIFDriver) ReleaseVIF(_ context.Context, v *kuryrv1.VIF, _ drivers.PodVIFRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, v.ID)
	return nil
}

func (f *fakeVIFDriver) ActivateVIF(context.Context, *kuryrv1.VIF, drivers.PodVIFRequest) error {
	return nil
}

func (f *fakeVIFDriver) UpdateVIFSecurityGroups(_ context.Context, v *kuryrv1.VIF, sgs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sgUpdates[v.ID] = append([]string(nil), sgs...)
	return nil
}

func (f *fakeVIFDriver) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

func (f *fakeVIFDriver) releasedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.released...)
}

// --- fake cloud --------------------------------------------------------

type fakeCloud struct {
	mu      sync.Mutex
	ports   map[string]*openstack.Port
	trunks  []*openstack.Trunk
	deleted []string
	tags    []string
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{ports: map[string]*openstack.Port{}}
}

func (c *fakeCloud) ResourceTags() []string { return c.tags }

func (c *fakeCloud) ListPorts(_ context.Context, f openstack.PortFilter) ([]*openstack.Port, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*openstack.Port
	for _, p := range c.ports {
		if f.DeviceOwner != "" && p.DeviceOwner != f.DeviceOwner {
			continue
		}
		if f.Name != "" && p.Name != f.Name {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (c *fakeCloud) GetPort(_ context.Context, id string) (*openstack.Port, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.ports[id]
	if !ok {
		return nil, fmt.Errorf("port %s not found", id)
	}
	return p, nil
}

func (c *fakeCloud) UpdatePort(_ context.Context, id string, u openstack.PortUpdate) (*openstack.Port, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.ports[id]
	if !ok {
		return nil, fmt.Errorf("port %s not found", id)
	}
	if u.Name != nil {
		p.Name = *u.Name
	}
	if u.SecurityGroups != nil {
		p.SecurityGroups = *u.SecurityGroups
	}
	return p, nil
}

func (c *fakeCloud) DeletePort(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.ports, id)
	c.deleted = append(c.deleted, id)
	return nil
}

func (c *fakeCloud) GetSubnet(_ context.Context, id string) (*openstack.Subnet, error) {
	return &openstack.Subnet{ID: id, NetworkID: testNetID, CIDR: "10.1.0.0/24", GatewayIP: "10.1.0.1"}, nil
}

func (c *fakeCloud) GetNetwork(_ context.Context, id string) (*openstack.Network, error) {
	return &openstack.Network{ID: id, Name: "pod-net", MTU: 1442}, nil
}

func (c *fakeCloud) ListTrunks(context.Context) ([]*openstack.Trunk, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.trunks, nil
}

// addPort registers a Neutron port the pool may discover or read back. The
// device owner is always openstack.DeviceOwner: a port with any other owner
// is not one the pool would ever claim.
func (c *fakeCloud) addPort(id string, sgs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ports[id] = &openstack.Port{
		ID:             id,
		NetworkID:      testNetID,
		ProjectID:      testProj,
		Name:           openstack.PoolPortName,
		Status:         openstack.PortStatusActive,
		MACAddress:     "fa:16:3e:00:00:01",
		DeviceOwner:    openstack.DeviceOwner,
		HostID:         testNode,
		VIFType:        "ovs",
		SecurityGroups: sgs,
		FixedIPs:       []openstack.FixedIP{{SubnetID: testSubID, IPAddress: "10.1.0.5"}},
	}
}

// --- harness -----------------------------------------------------------

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := kuryrv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestPool(t *testing.T, tweak func(*config.Config), objs ...client.Object) (*Pool, *fakeVIFDriver, *fakeCloud) {
	t.Helper()

	cfg := config.Defaults()
	cfg.NeutronDefaults.Project = testProj
	cfg.NeutronDefaults.PodSubnet = testSubID
	cfg.VIFPool.PortsPoolMin = 3
	cfg.VIFPool.PortsPoolBatch = 3
	cfg.VIFPool.PortsPoolMax = 0
	if tweak != nil {
		tweak(cfg)
	}

	inner := newFakeDriver("neutron-vif")
	cl := newFakeCloud()
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()

	p, err := New(cfg, cl, k8s, inner, NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, inner, cl
}

func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default", UID: types.UID("uid-1")},
		Spec:       corev1.PodSpec{NodeName: testNode},
		Status:     corev1.PodStatus{HostIP: testHostIP},
	}
}

func testRequest(sgs ...string) drivers.PodVIFRequest {
	return drivers.PodVIFRequest{
		Pod:       testPod(),
		ProjectID: testProj,
		Subnets: vif.SubnetSet{
			testSubID: {ID: testSubID, NetworkID: testNetID, CIDR: "10.1.0.0/24"},
		},
		SecurityGroups: sgs,
	}
}

// --- tests -------------------------------------------------------------

func TestRefusesToServeBeforeRecovery(t *testing.T) {
	// Serving before recovery would create duplicates of ports that already
	// exist in Neutron, which is how a restart loop exhausts project quota.
	p, inner, _ := newTestPool(t, nil)

	_, err := p.RequestVIF(context.Background(), testRequest("sg-a"))
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	if inner.createdCount() != 0 {
		t.Errorf("created %d ports before recovery, want 0", inner.createdCount())
	}
}

func TestFirstRequestFillsThePool(t *testing.T) {
	p, inner, _ := newTestPool(t, nil)
	ctx := context.Background()
	if err := p.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	v, err := p.RequestVIF(ctx, testRequest("sg-a"))
	if err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	if v == nil {
		t.Fatal("expected a VIF")
	}

	// One bulk create of ports_pool_batch, one port handed out.
	if got := inner.createdCount(); got != 3 {
		t.Errorf("created %d ports, want 3 (one batch)", got)
	}
	if got := totalOf(p.Stats()); got != 2 {
		t.Errorf("pool holds %d ports, want 2 left after serving one", got)
	}
}

func TestSubsequentRequestsAreServedFromThePool(t *testing.T) {
	// A batch of ten against a min of one is what makes the count below
	// deterministic. Every request that is served from the pool kicks off a
	// background top-up, and that top-up creates a whole batch whenever the
	// pool has fallen below ports_pool_min. Taking six ports out of ten
	// leaves four, which is still above the min, so those goroutines find
	// nothing to do. Setting min to the batch size instead -- as this test
	// once did -- puts the pool under the min from the very first take, and
	// the assertion then races the batch those goroutines create.
	p, inner, _ := newTestPool(t, func(c *config.Config) {
		c.VIFPool.PortsPoolMin = 1
		c.VIFPool.PortsPoolBatch = 10
	})
	ctx := context.Background()
	if err := p.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if _, err := p.RequestVIF(ctx, testRequest("sg-a")); err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	afterFirst := inner.createdCount()
	if afterFirst != 10 {
		t.Fatalf("first request created %d ports, want 10 (one batch)", afterFirst)
	}

	for i := 0; i < 5; i++ {
		if _, err := p.RequestVIF(ctx, testRequest("sg-a")); err != nil {
			t.Fatalf("RequestVIF %d: %v", i, err)
		}
	}

	// The pool still has spare ports, so no further creation was needed on
	// the hot path. This is the whole point of pooling.
	if got := inner.createdCount(); got != afterFirst {
		t.Errorf("created %d ports total, want %d; pods should be served from the pool",
			got, afterFirst)
	}
	if got := totalOf(p.Stats()); got != 4 {
		t.Errorf("pool holds %d ports, want 4 left after serving six", got)
	}
}

// TestPoolTopsUpInTheBackground covers the other half of the contract: a pod
// is served immediately and the refill happens behind it. The refill is what
// made the test above flaky before its min was lowered, so it is worth
// pinning down deliberately rather than tripping over by accident.
func TestPoolTopsUpInTheBackground(t *testing.T) {
	// With ports_pool_min equal to the batch, handing out a single port puts
	// the pool under the min, which is what schedules a fresh batch.
	p, inner, _ := newTestPool(t, func(c *config.Config) {
		c.VIFPool.PortsPoolMin = 5
		c.VIFPool.PortsPoolBatch = 5
	})
	ctx := context.Background()
	if err := p.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The first request fills the pool synchronously and takes one port. That
	// path returns without scheduling anything, so the count is settled here.
	if _, err := p.RequestVIF(ctx, testRequest("sg-a")); err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	if got := inner.createdCount(); got != 5 {
		t.Fatalf("first request created %d ports, want 5 (one batch)", got)
	}

	// The second is served from the pool, leaving three of five, and schedules
	// the top-up. Polling is the honest way to assert on it: the whole point
	// is that the pod did not wait for it.
	if _, err := p.RequestVIF(ctx, testRequest("sg-a")); err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for inner.createdCount() != 10 {
		if time.Now().After(deadline) {
			t.Fatalf("background top-up created %d ports in total, want 10 (a second batch)",
				inner.createdCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBorrowsFromAnotherSecurityGroupWhenNeeded(t *testing.T) {
	p, inner, _ := newTestPool(t, nil)
	ctx := context.Background()
	_ = p.Recover(ctx)

	// Fill the pool with ports carrying sg-a.
	if _, err := p.RequestVIF(ctx, testRequest("sg-a")); err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}

	// A pod wanting sg-b must be served by reusing an sg-a port and
	// rewriting its groups, not by leaving the pool idle and creating more.
	v, err := p.RequestVIF(ctx, testRequest("sg-b"))
	if err != nil {
		t.Fatalf("RequestVIF sg-b: %v", err)
	}

	inner.mu.Lock()
	applied, ok := inner.sgUpdates[v.ID]
	inner.mu.Unlock()
	if !ok {
		t.Fatalf("port %s was served without having its security groups rewritten", v.ID)
	}
	if len(applied) != 1 || applied[0] != "sg-b" {
		t.Errorf("applied groups = %v, want [sg-b]", applied)
	}
}

func TestReleaseDoesNotCallNeutron(t *testing.T) {
	p, inner, _ := newTestPool(t, nil)
	ctx := context.Background()
	_ = p.Recover(ctx)

	v, err := p.RequestVIF(ctx, testRequest("sg-a"))
	if err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	if err := p.ReleaseVIF(ctx, v, testRequest("sg-a")); err != nil {
		t.Fatalf("ReleaseVIF: %v", err)
	}

	// Deleting a pod must not block on Neutron.
	if got := inner.releasedIDs(); len(got) != 0 {
		t.Errorf("released %v to Neutron; a pooled port should only be queued", got)
	}
}

func TestRecycleReturnsPortsToThePool(t *testing.T) {
	p, inner, cl := newTestPool(t, nil)
	ctx := context.Background()
	_ = p.Recover(ctx)

	v, err := p.RequestVIF(ctx, testRequest("sg-a"))
	if err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	// Neutron must know about the port for the security group read-back.
	cl.addPort(v.ID, []string{"sg-a"})

	before := totalOf(p.Stats())
	if err := p.ReleaseVIF(ctx, v, testRequest("sg-a")); err != nil {
		t.Fatalf("ReleaseVIF: %v", err)
	}
	if err := p.Recycle(ctx); err != nil {
		t.Fatalf("Recycle: %v", err)
	}

	if after := totalOf(p.Stats()); after != before+1 {
		t.Errorf("pool holds %d ports after recycling, want %d", after, before+1)
	}
	if got := inner.releasedIDs(); len(got) != 0 {
		t.Errorf("recycling deleted %v; the port should have been reused", got)
	}
}

func TestRecycleRereadsSecurityGroups(t *testing.T) {
	// A NetworkPolicy can rewrite the groups on a port while a pod holds it.
	// Filing it back under the groups it had when it was handed out would
	// give the next pod the wrong ones.
	p, _, cl := newTestPool(t, nil)
	ctx := context.Background()
	_ = p.Recover(ctx)

	v, err := p.RequestVIF(ctx, testRequest("sg-a"))
	if err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	cl.addPort(v.ID, []string{"sg-POLICY"})

	if err := p.ReleaseVIF(ctx, v, testRequest("sg-a")); err != nil {
		t.Fatalf("ReleaseVIF: %v", err)
	}
	if err := p.Recycle(ctx); err != nil {
		t.Fatalf("Recycle: %v", err)
	}

	k := key{Host: testNode, ProjectID: testProj, NetworkID: testNetID}
	p.mu.Lock()
	b := p.available[k]
	filed := ""
	for sg, ports := range b.groups {
		for _, id := range ports {
			if id == v.ID {
				filed = string(sg)
			}
		}
	}
	p.mu.Unlock()

	if filed != "sg-POLICY" {
		t.Errorf("port filed under groups %q, want sg-POLICY as read back from Neutron", filed)
	}
}

func TestRecycleDeletesWhenPoolIsFull(t *testing.T) {
	p, inner, cl := newTestPool(t, func(c *config.Config) {
		c.VIFPool.PortsPoolMin = 2
		c.VIFPool.PortsPoolBatch = 2
		c.VIFPool.PortsPoolMax = 2
	})
	ctx := context.Background()
	_ = p.Recover(ctx)

	v, err := p.RequestVIF(ctx, testRequest("sg-a"))
	if err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	cl.addPort(v.ID, []string{"sg-a"})

	// Top the pool back up to its maximum before returning the port.
	if err := p.populate(ctx, key{Host: testNode, ProjectID: testProj, NetworkID: testNetID},
		testRequest("sg-a")); err != nil {
		t.Fatalf("populate: %v", err)
	}
	if got := totalOf(p.Stats()); got < 2 {
		t.Fatalf("pool holds %d ports, expected it to be at its maximum of 2", got)
	}

	if err := p.ReleaseVIF(ctx, v, testRequest("sg-a")); err != nil {
		t.Fatalf("ReleaseVIF: %v", err)
	}
	if err := p.Recycle(ctx); err != nil {
		t.Fatalf("Recycle: %v", err)
	}

	// Over the maximum, so the surplus port is deleted through the wrapped
	// driver, which is what detaches a subport in the nested case.
	released := inner.releasedIDs()
	if len(released) != 1 || released[0] != v.ID {
		t.Errorf("released = %v, want [%s]", released, v.ID)
	}
}

func TestPoolsAreIsolatedPerNode(t *testing.T) {
	p, _, _ := newTestPool(t, nil)
	ctx := context.Background()
	_ = p.Recover(ctx)

	reqA := testRequest("sg-a")
	reqB := testRequest("sg-a")
	reqB.Pod = testPod()
	reqB.Pod.Spec.NodeName = "worker-2"

	if _, err := p.RequestVIF(ctx, reqA); err != nil {
		t.Fatalf("RequestVIF worker-1: %v", err)
	}
	if _, err := p.RequestVIF(ctx, reqB); err != nil {
		t.Fatalf("RequestVIF worker-2: %v", err)
	}

	stats := p.Stats()
	if len(stats) != 2 {
		t.Fatalf("got %d pools, want one per node: %v", len(stats), stats)
	}
}

func TestRecoverRediscoversPooledPorts(t *testing.T) {
	// A controller restart must find the ports its predecessor pre-created,
	// or it creates a second set and doubles the quota footprint.
	p, inner, cl := newTestPool(t, nil)
	cl.addPort("leftover-1", []string{"sg-a"})
	cl.addPort("leftover-2", []string{"sg-a"})

	ctx := context.Background()
	if err := p.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if got := totalOf(p.Stats()); got != 2 {
		t.Fatalf("recovered %d ports, want 2", got)
	}

	// Serving from the recovered pool must not create anything new.
	if _, err := p.RequestVIF(ctx, testRequest("sg-a")); err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	if got := inner.createdCount(); got != 0 {
		t.Errorf("created %d ports after recovery, want 0; the recovered ones should be used", got)
	}
}

func TestRecoverSkipsPortsInUse(t *testing.T) {
	// A port recorded on a live KuryrPort belongs to a running pod and must
	// not be handed to another one.
	inUse := &kuryrv1.KuryrPort{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Spec:       kuryrv1.KuryrPortSpec{PodUID: "uid-1", PodNodeName: testNode},
		Status: kuryrv1.KuryrPortStatus{
			VIFs: map[string]kuryrv1.PortVIF{
				kuryrv1.DefaultIfName: {Default: true, VIF: kuryrv1.VIF{ID: "leftover-1"}},
			},
		},
	}

	p, _, cl := newTestPool(t, nil, inUse)
	cl.addPort("leftover-1", []string{"sg-a"})
	cl.addPort("leftover-2", []string{"sg-a"})

	if err := p.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := totalOf(p.Stats()); got != 1 {
		t.Errorf("recovered %d ports, want 1; the in-use port must be skipped", got)
	}
}

func TestRecoverDeletesUnplaceablePorts(t *testing.T) {
	// A port with no binding information cannot be assigned to a pool and
	// would never reach ACTIVE. Leaving it would burn quota forever.
	p, _, cl := newTestPool(t, nil)
	cl.addPort("orphan", []string{"sg-a"})
	cl.mu.Lock()
	cl.ports["orphan"].HostID = ""
	cl.ports["orphan"].VIFType = openstack.VIFTypeUnbound
	cl.mu.Unlock()

	if err := p.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := totalOf(p.Stats()); got != 0 {
		t.Errorf("pooled %d unplaceable ports, want 0", got)
	}

	cl.mu.Lock()
	deleted := append([]string(nil), cl.deleted...)
	cl.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "orphan" {
		t.Errorf("deleted = %v, want [orphan]", deleted)
	}
}

func TestDeleteNetworkPoolsReleasesPorts(t *testing.T) {
	// Namespace teardown has to empty the pool first: Neutron refuses to
	// delete a network that still has ports on it.
	p, inner, _ := newTestPool(t, nil)
	ctx := context.Background()
	_ = p.Recover(ctx)

	if _, err := p.RequestVIF(ctx, testRequest("sg-a")); err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	pooled := totalOf(p.Stats())
	if pooled == 0 {
		t.Fatal("expected the pool to hold spare ports")
	}

	if err := p.DeleteNetworkPools(ctx, testNetID); err != nil {
		t.Fatalf("DeleteNetworkPools: %v", err)
	}
	if got := totalOf(p.Stats()); got != 0 {
		t.Errorf("pool holds %d ports after network deletion, want 0", got)
	}
	if got := len(inner.releasedIDs()); got != pooled {
		t.Errorf("released %d ports, want %d", got, pooled)
	}
}

func TestConcurrentRequestsGetDistinctPorts(t *testing.T) {
	p, _, _ := newTestPool(t, func(c *config.Config) {
		c.VIFPool.PortsPoolMin = 20
		c.VIFPool.PortsPoolBatch = 20
	})
	ctx := context.Background()
	_ = p.Recover(ctx)

	const n = 20
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got []string
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := p.RequestVIF(ctx, testRequest("sg-a"))
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, v.ID)
			mu.Unlock()
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("port %s was handed to two pods", id)
		}
		seen[id] = true
	}
	if len(got) != n {
		t.Errorf("served %d of %d requests", len(got), n)
	}
}

func TestBucketPrefersExactSecurityGroupMatch(t *testing.T) {
	b := newBucket()
	b.push("sg-a", "port-a")
	b.push("sg-b", "port-b")

	taken, ok := b.pop("sg-b")
	if !ok || taken.portID != "port-b" {
		t.Fatalf("pop returned %q, %v; want port-b", taken.portID, ok)
	}
	if taken.needsSGUpdate {
		t.Error("an exact match must not require a security group rewrite")
	}
	if taken.held != "sg-b" {
		t.Errorf("pop reported the port holds %q, want sg-b", taken.held)
	}
}

func TestBucketBorrowsFromOldestGroup(t *testing.T) {
	b := newBucket()
	b.push("sg-old", "port-old")
	b.push("sg-new", "port-new")

	// sg-old was touched first, so it is at the front and gets raided first.
	taken, ok := b.pop("sg-absent")
	if !ok {
		t.Fatal("expected a port to be borrowed")
	}
	if taken.portID != "port-old" {
		t.Errorf("borrowed %q, want port-old (the least recently refreshed group)", taken.portID)
	}
	if !taken.needsSGUpdate {
		t.Error("a borrowed port must be flagged for a security group rewrite")
	}
	// The groups the port still carries are what it has to go back under if
	// the rewrite fails, so pop has to report them rather than the ones the
	// caller asked for.
	if taken.held != "sg-old" {
		t.Errorf("pop reported the port holds %q, want sg-old", taken.held)
	}
}

func TestStrategyRejectsUnpoolableDriver(t *testing.T) {
	cfg := config.Defaults()
	cfg.NeutronDefaults.Project = testProj
	cfg.NeutronDefaults.PodSubnet = testSubID

	for _, name := range []string{"nested-dpdk", "nested-macvlan"} {
		_, err := New(cfg, newFakeCloud(), fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
			newFakeDriver(name), nil)
		if err == nil {
			t.Errorf("expected pooling to be refused for %s, which it cannot support", name)
		}
	}
}

// Priming is what makes the first pod in a new namespace cheap: without it
// that pod pays for a synchronous bulk create.
func TestPopulatePrimesThePoolForANode(t *testing.T) {
	p, inner, _ := newTestPool(t, func(c *config.Config) {
		c.VIFPool.PortsPoolMin = 1
		c.VIFPool.PortsPoolBatch = 4
	})
	ctx := context.Background()
	if err := p.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := inner.createdCount(); got != 0 {
		t.Fatalf("created %d ports before priming, want 0", got)
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNode}}
	if err := p.Populate(ctx, node, testRequest("sg-a")); err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if got := inner.createdCount(); got != 4 {
		t.Fatalf("priming created %d ports, want 4 (one batch)", got)
	}
	if got := totalOf(p.Stats()); got != 4 {
		t.Fatalf("pool holds %d ports after priming, want 4", got)
	}

	// The pod that follows is served from what priming left, without another
	// create. That is the whole point.
	if _, err := p.RequestVIF(ctx, testRequest("sg-a")); err != nil {
		t.Fatalf("RequestVIF: %v", err)
	}
	if got := inner.createdCount(); got != 4 {
		t.Errorf("created %d ports in total, want 4; the pod should have come from the pool", got)
	}
}

// Priming twice would create a second batch on every node, so a pool that is
// already at ports_pool_min has to be left alone.
func TestPopulateIsANoOpWhenThePoolIsStocked(t *testing.T) {
	p, inner, _ := newTestPool(t, func(c *config.Config) {
		c.VIFPool.PortsPoolMin = 1
		c.VIFPool.PortsPoolBatch = 4
	})
	ctx := context.Background()
	if err := p.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNode}}
	if err := p.Populate(ctx, node, testRequest("sg-a")); err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if err := p.Populate(ctx, node, testRequest("sg-a")); err != nil {
		t.Fatalf("Populate again: %v", err)
	}
	if got := inner.createdCount(); got != 4 {
		t.Errorf("created %d ports over two primes, want 4", got)
	}
}

// A node with no name cannot key a pool, and priming against the wrong key
// would create ports no pod ever finds.
func TestPopulateRejectsANamelessNode(t *testing.T) {
	p, _, _ := newTestPool(t, nil)
	if err := p.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := p.Populate(context.Background(), &corev1.Node{}, testRequest("sg-a")); err == nil {
		t.Error("expected an error priming against a node with no name")
	}
}
