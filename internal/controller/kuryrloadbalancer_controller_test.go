package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

// fakeLBCloud is an Octavia that answers immediately.
//
// Real Octavia provisions asynchronously; that path is exercised by
// TestKLBWaitsWhileProvisioning, which drives the status by hand. Everything
// else is easier to read against a cloud that just works.
type fakeLBCloud struct {
	mu sync.Mutex

	lbs       map[string]*openstack.LoadBalancer
	listeners map[string]*openstack.Listener
	pools     map[string]*openstack.Pool
	members   map[string]*openstack.Member
	ports     map[string]*openstack.Port
	subnets   map[string]*openstack.Subnet
	sgRules   map[string]*openstack.SGRule
	next      int

	// createStatus is what a newly created load balancer reports.
	createStatus string

	createLBErr       error
	createListenerErr error
	createMemberErr   error
	deleteLBErr       error
}

func newFakeLBCloud() *fakeLBCloud {
	return &fakeLBCloud{
		lbs:       map[string]*openstack.LoadBalancer{},
		listeners: map[string]*openstack.Listener{},
		pools:     map[string]*openstack.Pool{},
		members:   map[string]*openstack.Member{},
		ports:     map[string]*openstack.Port{},
		subnets: map[string]*openstack.Subnet{
			"service-subnet": {ID: "service-subnet", CIDR: "10.96.0.0/12"},
		},
		sgRules:      map[string]*openstack.SGRule{},
		createStatus: openstack.ProvisioningActive,
	}
}

func (f *fakeLBCloud) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s-%d", prefix, f.next)
}

func (f *fakeLBCloud) CreateLoadBalancer(_ context.Context, spec openstack.LoadBalancerSpec) (*openstack.LoadBalancer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createLBErr; err != nil {
		f.createLBErr = nil
		return nil, err
	}
	portID := f.id("vip-port")
	lb := &openstack.LoadBalancer{
		ID:                 f.id("lb"),
		Name:               spec.Name,
		ProjectID:          spec.ProjectID,
		VIPAddress:         spec.VIPAddress,
		VIPSubnetID:        spec.SubnetID,
		VIPPortID:          portID,
		Provider:           spec.Provider,
		ProvisioningStatus: f.createStatus,
	}
	f.lbs[lb.ID] = lb
	f.ports[portID] = &openstack.Port{ID: portID}
	return lb, nil
}

func (f *fakeLBCloud) GetLoadBalancer(_ context.Context, id string) (*openstack.LoadBalancer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb, ok := f.lbs[id]
	if !ok {
		return nil, errNotFound
	}
	copied := *lb
	return &copied, nil
}

func (f *fakeLBCloud) FindLoadBalancersByName(_ context.Context, name, _ string) ([]*openstack.LoadBalancer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*openstack.LoadBalancer
	for _, lb := range f.lbs {
		if lb.Name == name {
			out = append(out, lb)
		}
	}
	return out, nil
}

func (f *fakeLBCloud) DeleteLoadBalancer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.deleteLBErr; err != nil {
		f.deleteLBErr = nil
		return err
	}
	delete(f.lbs, id)
	// The real delete cascades.
	for lid, l := range f.listeners {
		if l.LoadBalancerID == id {
			delete(f.listeners, lid)
		}
	}
	for pid, p := range f.pools {
		if p.LoadBalancerID == id {
			delete(f.pools, pid)
		}
	}
	f.members = map[string]*openstack.Member{}
	return nil
}

func (f *fakeLBCloud) CreateListener(_ context.Context, l openstack.Listener, _, _ int32) (*openstack.Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createListenerErr; err != nil {
		f.createListenerErr = nil
		return nil, err
	}
	l.ID = f.id("listener")
	f.listeners[l.ID] = &l
	return &l, nil
}

func (f *fakeLBCloud) DeleteListener(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.listeners, id)
	return nil
}

func (f *fakeLBCloud) CreatePool(_ context.Context, p openstack.Pool) (*openstack.Pool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p.ID = f.id("pool")
	f.pools[p.ID] = &p
	return &p, nil
}

func (f *fakeLBCloud) DeletePool(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pools, id)
	for mid, m := range f.members {
		if m.PoolID == id {
			delete(f.members, mid)
		}
	}
	return nil
}

func (f *fakeLBCloud) CreateMember(_ context.Context, m openstack.Member) (*openstack.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createMemberErr; err != nil {
		f.createMemberErr = nil
		return nil, err
	}
	m.ID = f.id("member")
	f.members[m.ID] = &m
	return &m, nil
}

func (f *fakeLBCloud) DeleteMember(_ context.Context, _, memberID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.members, memberID)
	return nil
}

func (f *fakeLBCloud) GetPort(_ context.Context, id string) (*openstack.Port, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	port, ok := f.ports[id]
	if !ok {
		return nil, errNotFound
	}
	copied := *port
	return &copied, nil
}

func (f *fakeLBCloud) GetSubnet(_ context.Context, id string) (*openstack.Subnet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	subnet, ok := f.subnets[id]
	if !ok {
		return nil, errNotFound
	}
	return subnet, nil
}

func (f *fakeLBCloud) ListSGRules(_ context.Context, sgID string) ([]*openstack.SGRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*openstack.SGRule
	for _, r := range f.sgRules {
		if r.SecurityGroupID == sgID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeLBCloud) CreateSGRule(_ context.Context, r openstack.SGRule) (*openstack.SGRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ID = f.id("rule")
	f.sgRules[r.ID] = &r
	return &r, nil
}

func (f *fakeLBCloud) DeleteSGRule(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sgRules, id)
	return nil
}

func (f *fakeLBCloud) counts() (lbs, listeners, pools, members int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lbs), len(f.listeners), len(f.pools), len(f.members)
}

// errNotFound is the shape a gophercloud 404 has, which is what
// openstack.IsNotFound recognises. A plain sentinel would be reported as an
// unexpected failure instead of a missing resource.
var errNotFound = gophercloud.ErrUnexpectedResponseCode{Actual: 404, Expected: []int{200}}

// fakePublicIP hands out predictable floating IPs.
type fakePublicIP struct {
	acquired   []string
	released   []string
	associated []string
	acquireErr error
	nextID     int
}

func (f *fakePublicIP) Acquire(_ context.Context, klb *kuryrv1.KuryrLoadBalancer, _, portID string) (*drivers.PublicIP, error) {
	if f.acquireErr != nil {
		return nil, f.acquireErr
	}
	if klb.Spec.Type != string(corev1.ServiceTypeLoadBalancer) {
		return nil, nil
	}
	f.nextID++
	ip := &drivers.PublicIP{
		ID:       fmt.Sprintf("fip-%d", f.nextID),
		Address:  fmt.Sprintf("192.0.2.%d", f.nextID),
		FromPool: true,
	}
	f.acquired = append(f.acquired, ip.ID)
	return ip, nil
}

func (f *fakePublicIP) Associate(_ context.Context, ip *drivers.PublicIP, portID string) error {
	f.associated = append(f.associated, ip.ID+"->"+portID)
	return nil
}

func (f *fakePublicIP) Release(_ context.Context, ip *drivers.PublicIP) error {
	f.released = append(f.released, ip.ID)
	return nil
}

func newKLBObject(mods ...func(*kuryrv1.KuryrLoadBalancer)) *kuryrv1.KuryrLoadBalancer {
	klb := &kuryrv1.KuryrLoadBalancer{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "web",
			Namespace:  "default",
			UID:        types.UID("klb-uid-1"),
			Finalizers: []string{kuryrv1.KuryrLoadBalancerFinalizer},
		},
		Spec: kuryrv1.KuryrLoadBalancerSpec{
			ProjectID: "project-1",
			SubnetID:  "service-subnet",
			IP:        "10.96.0.10",
			Type:      string(corev1.ServiceTypeClusterIP),
			Ports: []kuryrv1.ServicePort{{
				Name:       "http",
				Protocol:   corev1.ProtocolTCP,
				Port:       80,
				TargetPort: "web-port",
			}},
			Subsets: []kuryrv1.EndpointSubset{{
				Addresses: []kuryrv1.EndpointAddress{
					{IP: "10.0.1.5", Ready: true},
					{IP: "10.0.1.6", Ready: true},
				},
				Ports: []kuryrv1.ServicePort{{
					Name: "http", Protocol: corev1.ProtocolTCP, Port: 8080,
				}},
			}},
		},
	}
	for _, m := range mods {
		m(klb)
	}
	return klb
}

func newKLBReconciler(t *testing.T, cloud loadBalancerCloud, publicIP drivers.PublicIPDriver,
	objs ...client.Object) *KuryrLoadBalancerReconciler {

	t.Helper()
	scheme := testScheme(t)
	cfg := config.Defaults()
	cfg.OctaviaDefaults.EnforceSGRules = false

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&kuryrv1.KuryrLoadBalancer{}, &corev1.Service{}).
		Build()

	return &KuryrLoadBalancerReconciler{
		Client:    c,
		Scheme:    scheme,
		Config:    cfg,
		OpenStack: cloud,
		PublicIP:  publicIP,
	}
}

func reconcileKLB(t *testing.T, r *KuryrLoadBalancerReconciler, klb *kuryrv1.KuryrLoadBalancer) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: klb.Namespace, Name: klb.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return result
}

func readKLBObject(t *testing.T, r *KuryrLoadBalancerReconciler, klb *kuryrv1.KuryrLoadBalancer) *kuryrv1.KuryrLoadBalancer {
	t.Helper()
	var out kuryrv1.KuryrLoadBalancer
	key := types.NamespacedName{Namespace: klb.Namespace, Name: klb.Name}
	if err := r.Get(context.Background(), key, &out); err != nil {
		t.Fatalf("reading the KuryrLoadBalancer: %v", err)
	}
	return &out
}

// settle runs the reconciler until it stops asking to be requeued, which is
// how the asynchronous Octavia provisioning is driven in a test.
func settle(t *testing.T, r *KuryrLoadBalancerReconciler, klb *kuryrv1.KuryrLoadBalancer) {
	t.Helper()
	for i := 0; i < 10; i++ {
		result := reconcileKLB(t, r, klb)
		if result.IsZero() {
			return
		}
	}
	t.Fatal("the reconciler never settled")
}

func TestKLBBuildsTheWholeTopology(t *testing.T) {
	cloud := newFakeLBCloud()
	klb := newKLBObject()
	r := newKLBReconciler(t, cloud, nil, klb)

	settle(t, r, klb)

	lbs, listeners, pools, members := cloud.counts()
	if lbs != 1 || listeners != 1 || pools != 1 {
		t.Fatalf("Octavia has %d load balancers, %d listeners, %d pools; want 1 of each",
			lbs, listeners, pools)
	}
	if members != 2 {
		t.Errorf("Octavia has %d members, want one per ready backend", members)
	}

	got := readKLBObject(t, r, klb)
	if got.Status.LoadBalancer == nil {
		t.Fatal("no load balancer recorded")
	}
	if got.Status.LoadBalancer.IP != "10.96.0.10" {
		t.Errorf("recorded VIP = %q, want the clusterIP", got.Status.LoadBalancer.IP)
	}
	if len(got.Status.Members) != 2 {
		t.Fatalf("recorded %d members, want 2", len(got.Status.Members))
	}
	for _, m := range got.Status.Members {
		// The EndpointSlice resolves the named targetPort, so members land on
		// 8080 and not on the Service port 80.
		if m.Port != 8080 {
			t.Errorf("member %s is on port %d, want the resolved backend port 8080", m.IP, m.Port)
		}
		// L3 is the default member mode, so members sit on the LB's subnet.
		if m.SubnetID != "service-subnet" {
			t.Errorf("member %s is on subnet %q, want the load balancer subnet", m.IP, m.SubnetID)
		}
	}

	cond := findLBCondition(got, kuryrv1.KuryrLoadBalancerConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition = %v, want True", cond)
	}
}

func TestKLBSecondPassChangesNothing(t *testing.T) {
	cloud := newFakeLBCloud()
	klb := newKLBObject()
	r := newKLBReconciler(t, cloud, nil, klb)

	settle(t, r, klb)
	before := readKLBObject(t, r, klb)

	settle(t, r, klb)
	after := readKLBObject(t, r, klb)

	_, listeners, pools, members := cloud.counts()
	if listeners != 1 || pools != 1 || members != 2 {
		t.Errorf("after a no-op pass Octavia has %d listeners, %d pools, %d members; "+
			"want 1, 1, 2", listeners, pools, members)
	}
	if before.Status.Listeners[0].ID != after.Status.Listeners[0].ID {
		t.Error("the listener was recreated on a pass that changed nothing")
	}
	if len(after.Status.Members) != 2 {
		t.Fatalf("recorded %d members, want 2", len(after.Status.Members))
	}
	beforeIDs := map[string]bool{}
	for _, m := range before.Status.Members {
		beforeIDs[m.ID] = true
	}
	for _, m := range after.Status.Members {
		if !beforeIDs[m.ID] {
			t.Errorf("member %s was recreated on a pass that changed nothing", m.IP)
		}
	}
}

func TestKLBFollowsBackendChanges(t *testing.T) {
	cloud := newFakeLBCloud()
	klb := newKLBObject()
	r := newKLBReconciler(t, cloud, nil, klb)
	settle(t, r, klb)

	// One backend goes away, another arrives.
	current := readKLBObject(t, r, klb)
	current.Spec.Subsets[0].Addresses = []kuryrv1.EndpointAddress{
		{IP: "10.0.1.5", Ready: true},
		{IP: "10.0.1.7", Ready: true},
	}
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatalf("updating the spec: %v", err)
	}

	settle(t, r, klb)

	if _, _, _, members := cloud.counts(); members != 2 {
		t.Errorf("Octavia has %d members, want 2", members)
	}
	got := map[string]bool{}
	for _, m := range readKLBObject(t, r, klb).Status.Members {
		got[m.IP] = true
	}
	if !got["10.0.1.5"] || !got["10.0.1.7"] {
		t.Errorf("members = %v, want the current backends", got)
	}
	if got["10.0.1.6"] {
		t.Error("the removed backend is still a member")
	}
}

func TestKLBFollowsPortChanges(t *testing.T) {
	cloud := newFakeLBCloud()
	klb := newKLBObject()
	r := newKLBReconciler(t, cloud, nil, klb)
	settle(t, r, klb)

	// The Service is edited to expose a different port.
	current := readKLBObject(t, r, klb)
	current.Spec.Ports = []kuryrv1.ServicePort{{
		Name: "https", Protocol: corev1.ProtocolTCP, Port: 443, TargetPort: "web-port",
	}}
	current.Spec.Subsets[0].Ports = []kuryrv1.ServicePort{{
		Name: "https", Protocol: corev1.ProtocolTCP, Port: 8443,
	}}
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatalf("updating the spec: %v", err)
	}

	settle(t, r, klb)

	got := readKLBObject(t, r, klb)
	if len(got.Status.Listeners) != 1 || got.Status.Listeners[0].Port != 443 {
		t.Fatalf("listeners = %+v, want a single listener on 443", got.Status.Listeners)
	}
	if _, listeners, pools, members := cloud.counts(); listeners != 1 || pools != 1 || members != 2 {
		t.Errorf("Octavia has %d listeners, %d pools, %d members; want 1, 1, 2",
			listeners, pools, members)
	}
	for _, m := range got.Status.Members {
		if m.Port != 8443 {
			t.Errorf("member %s is on port %d, want 8443", m.IP, m.Port)
		}
	}
}

func TestKLBSkipsBackendsThatAreNotReady(t *testing.T) {
	cloud := newFakeLBCloud()
	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.Subsets[0].Addresses[1].Ready = false
	})
	r := newKLBReconciler(t, cloud, nil, klb)

	settle(t, r, klb)

	if _, _, _, members := cloud.counts(); members != 1 {
		t.Errorf("Octavia has %d members, want only the ready backend", members)
	}
}

func TestKLBWaitsWhileProvisioning(t *testing.T) {
	cloud := newFakeLBCloud()
	cloud.createStatus = openstack.ProvisioningPendingCreate
	klb := newKLBObject()
	r := newKLBReconciler(t, cloud, nil, klb)

	// First pass creates it; the second finds it still building.
	if result := reconcileKLB(t, r, klb); result.RequeueAfter == 0 {
		t.Error("the reconciler did not ask to be requeued after creating the load balancer")
	}
	if result := reconcileKLB(t, r, klb); result.RequeueAfter == 0 {
		t.Error("the reconciler did not wait for a load balancer still provisioning")
	}
	if _, listeners, _, _ := cloud.counts(); listeners != 0 {
		t.Errorf("built %d listeners on a load balancer that is not ACTIVE yet", listeners)
	}

	// Octavia finishes.
	cloud.mu.Lock()
	for _, lb := range cloud.lbs {
		lb.ProvisioningStatus = openstack.ProvisioningActive
	}
	cloud.mu.Unlock()

	settle(t, r, klb)
	if _, listeners, pools, members := cloud.counts(); listeners != 1 || pools != 1 || members != 2 {
		t.Errorf("Octavia has %d listeners, %d pools, %d members; want 1, 1, 2",
			listeners, pools, members)
	}
}

func TestKLBRebuildsWhenTheLoadBalancerVanishes(t *testing.T) {
	// Something outside Kuryr deleted it. The recorded listeners and members
	// went with it, so the status has to be forgotten before rebuilding.
	cloud := newFakeLBCloud()
	klb := newKLBObject()
	r := newKLBReconciler(t, cloud, nil, klb)
	settle(t, r, klb)

	cloud.mu.Lock()
	cloud.lbs = map[string]*openstack.LoadBalancer{}
	cloud.listeners = map[string]*openstack.Listener{}
	cloud.pools = map[string]*openstack.Pool{}
	cloud.members = map[string]*openstack.Member{}
	cloud.mu.Unlock()

	settle(t, r, klb)

	if lbs, listeners, pools, members := cloud.counts(); lbs != 1 || listeners != 1 ||
		pools != 1 || members != 2 {
		t.Errorf("after the rebuild Octavia has %d load balancers, %d listeners, %d pools, "+
			"%d members; want 1, 1, 1, 2", lbs, listeners, pools, members)
	}
}

func TestKLBAdoptsAnExistingLoadBalancer(t *testing.T) {
	cloud := newFakeLBCloud()
	klb := newKLBObject()
	r := newKLBReconciler(t, cloud, nil, klb)

	name := loadBalancerName(klb.Namespace, klb.Name, string(klb.UID))
	orphan, err := cloud.CreateLoadBalancer(context.Background(), openstack.LoadBalancerSpec{
		Name: name, ProjectID: "project-1", SubnetID: "service-subnet", VIPAddress: "10.96.0.10",
	})
	if err != nil {
		t.Fatalf("seeding the orphaned load balancer: %v", err)
	}

	settle(t, r, klb)

	if got := readKLBObject(t, r, klb).Status.LoadBalancer.ID; got != orphan.ID {
		t.Errorf("load balancer = %q, want the adopted %q", got, orphan.ID)
	}
	if lbs, _, _, _ := cloud.counts(); lbs != 1 {
		t.Errorf("Octavia has %d load balancers, want 1: a duplicate was created", lbs)
	}
}

func TestKLBAssociatesAndReleasesAPublicIP(t *testing.T) {
	cloud := newFakeLBCloud()
	fips := &fakePublicIP{}
	svc := newService(func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeLoadBalancer })
	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.Type = string(corev1.ServiceTypeLoadBalancer)
	})
	r := newKLBReconciler(t, cloud, fips, klb, svc)

	settle(t, r, klb)

	got := readKLBObject(t, r, klb)
	if got.Status.PublicIP == nil {
		t.Fatal("no public IP recorded for a LoadBalancer Service")
	}
	if len(fips.associated) != 1 {
		t.Errorf("associations = %v, want exactly one", fips.associated)
	}

	// The address must reach the Service, or kubectl shows <pending> forever.
	var current corev1.Service
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	if err := r.Get(context.Background(), key, &current); err != nil {
		t.Fatalf("reading the Service: %v", err)
	}
	if len(current.Status.LoadBalancer.Ingress) != 1 ||
		current.Status.LoadBalancer.Ingress[0].IP != got.Status.PublicIP.IPAddr {
		t.Errorf("Service ingress = %+v, want the floating IP %s",
			current.Status.LoadBalancer.Ingress, got.Status.PublicIP.IPAddr)
	}

	// Deleting releases both the address and the load balancer.
	if err := r.Delete(context.Background(), got); err != nil {
		t.Fatalf("deleting the KuryrLoadBalancer: %v", err)
	}
	reconcileKLB(t, r, klb)

	if len(fips.released) != 1 {
		t.Errorf("released = %v, want the floating IP given back", fips.released)
	}
	if lbs, _, _, _ := cloud.counts(); lbs != 0 {
		t.Errorf("Octavia still has %d load balancers after finalizing", lbs)
	}
	var out kuryrv1.KuryrLoadBalancer
	if err := r.Get(context.Background(), key, &out); err == nil {
		t.Errorf("the KuryrLoadBalancer still exists with finalizers %v", out.Finalizers)
	}
}

func TestKLBRefusesLoadBalancerServiceWithoutExternalNetwork(t *testing.T) {
	// Silently leaving the Service without an address would look like a
	// cluster fault rather than a missing setting.
	cloud := newFakeLBCloud()
	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.Type = string(corev1.ServiceTypeLoadBalancer)
	})
	r := newKLBReconciler(t, cloud, nil, klb)

	// Get past the asynchronous creation first.
	reconcileKLB(t, r, klb)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: klb.Namespace, Name: klb.Name},
	})
	if err == nil {
		t.Fatal("expected an error for a LoadBalancer Service with no external network")
	}

	cond := findLBCondition(readKLBObject(t, r, klb), kuryrv1.KuryrLoadBalancerConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready condition = %v, want False", cond)
	}
}

func TestKLBKeepsFinalizerWhenReleaseFails(t *testing.T) {
	cloud := newFakeLBCloud()
	klb := newKLBObject()
	r := newKLBReconciler(t, cloud, nil, klb)
	settle(t, r, klb)

	current := readKLBObject(t, r, klb)
	if err := r.Delete(context.Background(), current); err != nil {
		t.Fatalf("deleting the KuryrLoadBalancer: %v", err)
	}

	boom := errors.New("octavia is down")
	cloud.deleteLBErr = boom

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: klb.Namespace, Name: klb.Name},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Reconcile error = %v, want the injected failure", err)
	}

	got := readKLBObject(t, r, klb)
	if !controllerutil.ContainsFinalizer(got, kuryrv1.KuryrLoadBalancerFinalizer) {
		t.Error("the finalizer was removed even though the load balancer could not be deleted")
	}
	if lbs, _, _, _ := cloud.counts(); lbs != 1 {
		t.Errorf("Octavia has %d load balancers, want the undeleted 1", lbs)
	}
}

func TestKLBWaitsForAnIncompleteSpec(t *testing.T) {
	// The Service controller fills the spec in; until it has, there is
	// nothing to build and nothing to complain about.
	cloud := newFakeLBCloud()
	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.IP = ""
		klb.Spec.SubnetID = ""
	})
	r := newKLBReconciler(t, cloud, nil, klb)

	reconcileKLB(t, r, klb)

	if lbs, _, _, _ := cloud.counts(); lbs != 0 {
		t.Errorf("built %d load balancers from an incomplete spec", lbs)
	}
}

func TestBackendPortMatchesByName(t *testing.T) {
	subset := &kuryrv1.EndpointSubset{
		Ports: []kuryrv1.ServicePort{
			{Name: "http", Protocol: corev1.ProtocolTCP, Port: 8080},
			{Name: "metrics", Protocol: corev1.ProtocolTCP, Port: 9090},
		},
	}

	got, ok := backendPort(subset, kuryrv1.ServicePort{Name: "metrics", Protocol: corev1.ProtocolTCP})
	if !ok || got != 9090 {
		t.Errorf("backendPort(metrics) = %d, %v; want 9090, true", got, ok)
	}
	if _, ok := backendPort(subset, kuryrv1.ServicePort{Name: "absent", Protocol: corev1.ProtocolTCP}); ok {
		t.Error("a port name no backend serves was matched anyway")
	}
	// A protocol mismatch is not the same port even under the same name.
	if _, ok := backendPort(subset, kuryrv1.ServicePort{Name: "http", Protocol: corev1.ProtocolUDP}); ok {
		t.Error("a UDP service port matched a TCP backend port")
	}
}

func findLBCondition(klb *kuryrv1.KuryrLoadBalancer, condType string) *metav1.Condition {
	for i := range klb.Status.Conditions {
		if klb.Status.Conditions[i].Type == condType {
			return &klb.Status.Conditions[i]
		}
	}
	return nil
}

func TestKLBRecordsMembersCreatedBeforeAFailure(t *testing.T) {
	// A member created but not recorded is a member nothing will ever delete.
	cloud := newFakeLBCloud()
	klb := newKLBObject()
	r := newKLBReconciler(t, cloud, nil, klb)

	// Get the load balancer, listener and pool in place first.
	settle(t, r, klb)

	// Now add a third backend and make its creation fail.
	current := readKLBObject(t, r, klb)
	current.Spec.Subsets[0].Addresses = append(current.Spec.Subsets[0].Addresses,
		kuryrv1.EndpointAddress{IP: "10.0.1.9", Ready: true})
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatalf("updating the spec: %v", err)
	}
	boom := errors.New("octavia is busy")
	cloud.createMemberErr = boom

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: klb.Namespace, Name: klb.Name},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Reconcile error = %v, want the injected failure", err)
	}

	// The two members that already existed must still be recorded.
	got := readKLBObject(t, r, klb)
	if len(got.Status.Members) != 2 {
		t.Fatalf("recorded %d members, want the 2 that survived the failure",
			len(got.Status.Members))
	}

	// And the retry completes the set rather than starting over.
	settle(t, r, klb)
	if _, _, _, members := cloud.counts(); members != 3 {
		t.Errorf("Octavia has %d members after the retry, want 3", members)
	}
}

func TestKLBRecordsListenersCreatedBeforeAFailure(t *testing.T) {
	cloud := newFakeLBCloud()
	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.Ports = []kuryrv1.ServicePort{
			{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80, TargetPort: "web-port"},
			{Name: "https", Protocol: corev1.ProtocolTCP, Port: 443, TargetPort: "tls-port"},
		}
	})
	r := newKLBReconciler(t, cloud, nil, klb)

	// The first pass creates the load balancer and nothing else.
	reconcileKLB(t, r, klb)

	// The second builds listeners; fail the second one.
	cloud.createListenerErr = errors.New("octavia is busy")
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: klb.Namespace, Name: klb.Name},
	}); err == nil {
		t.Fatal("expected the injected listener failure to be reported")
	}

	// Whatever was created has to be recorded, or the retry leaks it.
	recorded := len(readKLBObject(t, r, klb).Status.Listeners)
	_, inCloud, _, _ := cloud.counts()
	if recorded != inCloud {
		t.Errorf("recorded %d listeners but Octavia has %d; the difference would leak",
			recorded, inCloud)
	}

	settle(t, r, klb)
	if _, listeners, pools, _ := cloud.counts(); listeners != 2 || pools != 2 {
		t.Errorf("after the retry Octavia has %d listeners and %d pools, want 2 and 2",
			listeners, pools)
	}
}

func TestKLBReportsAFloatingIPFailure(t *testing.T) {
	cloud := newFakeLBCloud()
	fips := &fakePublicIP{acquireErr: errors.New("no addresses left in the pool")}
	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.Type = string(corev1.ServiceTypeLoadBalancer)
	})
	r := newKLBReconciler(t, cloud, fips, klb)

	reconcileKLB(t, r, klb)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: klb.Namespace, Name: klb.Name},
	}); err == nil {
		t.Fatal("expected the floating IP failure to be reported")
	}

	got := readKLBObject(t, r, klb)
	if got.Status.PublicIP != nil {
		t.Error("a public IP was recorded even though acquiring it failed")
	}
	if len(fips.acquired) != 0 {
		t.Errorf("acquired = %v, want nothing", fips.acquired)
	}
	cond := findLBCondition(got, kuryrv1.KuryrLoadBalancerConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready condition = %v, want False", cond)
	}
}

func TestKLBRecordsBackendSecurityGroupsWithoutTouchingTheVIPPort(t *testing.T) {
	// Octavia owns the security group on the VIP port. Overwriting it would
	// cut the load balancer off from its own control plane, so the Service's
	// groups are only recorded, to derive the VIP rules from.
	cloud := newFakeLBCloud()
	klb := newKLBObject(func(klb *kuryrv1.KuryrLoadBalancer) {
		klb.Spec.SecurityGroupIDs = []string{"sg-b", "sg-a"}
	})
	r := newKLBReconciler(t, cloud, nil, klb)

	// Give the VIP port the group Octavia would have created.
	reconcileKLB(t, r, klb)
	portID := readKLBObject(t, r, klb).Status.LoadBalancer.PortID
	cloud.mu.Lock()
	cloud.ports[portID].SecurityGroups = []string{"octavia-vip-sg"}
	cloud.mu.Unlock()

	settle(t, r, klb)

	port, err := cloud.GetPort(context.Background(), portID)
	if err != nil {
		t.Fatalf("reading the VIP port: %v", err)
	}
	if len(port.SecurityGroups) != 1 || port.SecurityGroups[0] != "octavia-vip-sg" {
		t.Errorf("VIP port groups = %v, want Octavia's own group untouched", port.SecurityGroups)
	}

	// Sorted, so a reordered spec is not mistaken for a change.
	got := readKLBObject(t, r, klb).Status.LoadBalancer.SecurityGroups
	if len(got) != 2 || got[0] != "sg-a" || got[1] != "sg-b" {
		t.Errorf("recorded groups = %v, want [sg-a sg-b]", got)
	}
}

// A policy whose rules change must refresh the VIP rules of the load
// balancers in its namespace, and only those.
func TestKLBRefreshesOnPolicyChange(t *testing.T) {
	here := newKLBObject()
	elsewhere := newKLBObject(func(k *kuryrv1.KuryrLoadBalancer) { k.Namespace = "other" })
	r := newKLBReconciler(t, nil, nil, here, elsewhere)

	knp := &kuryrv1.KuryrNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "deny", Namespace: here.Namespace}}
	got := r.loadBalancersInNamespace(context.Background(), knp)
	if len(got) != 1 || got[0].Name != here.Name || got[0].Namespace != here.Namespace {
		t.Errorf("enqueued %v, want only %s/%s", got, here.Namespace, here.Name)
	}
}
