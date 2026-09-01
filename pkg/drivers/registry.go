// Package drivers holds the pluggable decisions Kuryr makes when mapping
// Kubernetes objects onto OpenStack ones: which project owns a resource,
// which subnet a pod gets an address from, which security groups apply, and
// how a port is actually created.
//
// The Python implementation loaded these through stevedore entry points,
// resolved at runtime from setup.cfg. Go has no equivalent, and does not need
// one: a driver registers itself in an init function and the binary only
// contains drivers that were compiled into it. The failure mode moves from a
// runtime ImportError to a link-time symbol, which is a better place for it.
package drivers

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

// Deps is what every driver is handed at construction. Passing one struct
// rather than a growing argument list keeps driver signatures stable as new
// drivers need new collaborators.
type Deps struct {
	Config    *config.Config
	OpenStack *openstack.Client
	// K8s is the controller-runtime client, which reads through the manager
	// cache. Drivers that look up KuryrNetwork or Namespace objects use it.
	K8s client.Client
}

// ProjectDriver decides which OpenStack project owns the resources created
// for a Kubernetes object.
type ProjectDriver interface {
	// ProjectFor returns the project ID for an object. The object is a Pod,
	// Service, Namespace or NetworkPolicy depending on which driver slot this
	// is filling.
	ProjectFor(ctx context.Context, obj client.Object) (string, error)
}

// SubnetDriver decides which subnets a port gets addresses from.
type SubnetDriver interface {
	// SubnetsFor returns the subnets to allocate from, keyed by subnet ID.
	SubnetsFor(ctx context.Context, obj client.Object, projectID string) (vif.SubnetSet, error)
}

// SecurityGroupsDriver decides which security groups apply to a port.
type SecurityGroupsDriver interface {
	// SecurityGroupsFor returns the security group IDs for an object.
	SecurityGroupsFor(ctx context.Context, obj client.Object, projectID string) ([]string, error)
}

// PodVIFRequest is everything a VIF driver needs to create ports.
type PodVIFRequest struct {
	Pod            *corev1.Pod
	ProjectID      string
	Subnets        vif.SubnetSet
	SecurityGroups []string
	// HostID identifies the node, or for nested drivers the node VM. It is
	// resolved by the driver itself, since what it means differs per driver.
	HostID string
}

// PodVIFDriver creates and destroys the Neutron ports behind pod interfaces.
type PodVIFDriver interface {
	// Name is the registered driver name, used in logs and metrics.
	Name() string

	// RequestVIF creates one port for a pod.
	RequestVIF(ctx context.Context, req PodVIFRequest) (*kuryrv1.VIF, error)

	// RequestVIFs creates count interchangeable, unbound ports for a pool.
	// Implementations should use a single bulk Neutron call; that is the
	// entire reason pools are worth having.
	RequestVIFs(ctx context.Context, req PodVIFRequest, count int) ([]*kuryrv1.VIF, error)

	// ReleaseVIF destroys the port behind a VIF.
	ReleaseVIF(ctx context.Context, v *kuryrv1.VIF, req PodVIFRequest) error

	// ActivateVIF checks whether Neutron has brought the port to ACTIVE and
	// updates v.Active. It returns ErrPortNotReady while the port is still
	// coming up, which the caller is expected to retry.
	ActivateVIF(ctx context.Context, v *kuryrv1.VIF, req PodVIFRequest) error

	// UpdateVIFSecurityGroups re-applies security groups to a live port,
	// which is what happens when a NetworkPolicy starts or stops matching a
	// pod that is already running.
	UpdateVIFSecurityGroups(ctx context.Context, v *kuryrv1.VIF, sgs []string) error
}

// ErrPortNotReady means a port exists but Neutron has not made it ACTIVE yet.
// It is the Go equivalent of the Python PortNotReady exception, and callers
// treat it as "requeue", not "fail".
type ErrPortNotReady struct {
	PortID string
	Status string
}

func (e *ErrPortNotReady) Unwrap() error { return ErrRetryLater }

func (e *ErrPortNotReady) Error() string {
	return fmt.Sprintf("port %s is %s, not ACTIVE yet", e.PortID, e.Status)
}

// --- registries --------------------------------------------------------

// registry maps driver names to the constructors that build them. There is
// one instance per driver slot, which is what makes a name unambiguous: the
// same "default" can mean the pod subnet in one slot and the service subnet
// in another without the two colliding.
//
// The type parameter is the driver interface the slot holds. It is the whole
// reason this type exists: every slot needs the same name/lock/lookup code,
// and generics let them share it instead of repeating a register/build pair
// each.
type registry[T any] struct {
	// kind names the slot in the "unknown driver" error, which is the only
	// place an operator ever sees it.
	kind string

	mu        sync.RWMutex
	factories map[string]func(Deps) (T, error)
}

func newRegistry[T any](kind string) *registry[T] {
	return &registry[T]{kind: kind, factories: map[string]func(Deps) (T, error){}}
}

// register adds a driver under a name, replacing any driver already there.
func (r *registry[T]) register(name string, f func(Deps) (T, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = f
}

// build constructs the named driver.
//
// The factory is called after the lock is released, so a driver that
// registers another one during construction cannot deadlock. The list of
// available names is only assembled on the failure path, which is the only
// path that reads it.
func (r *registry[T]) build(name string, deps Deps) (T, error) {
	r.mu.RLock()
	f, ok := r.factories[name]
	var available []string
	if !ok {
		available = slices.Sorted(maps.Keys(r.factories))
	}
	r.mu.RUnlock()

	if !ok {
		var zero T
		return zero, unknownDriver(r.kind, name, available)
	}
	return f(deps)
}

// The driver slots.
//
// Services get their own subnet and security group registries even though the
// interfaces are identical. A driver named "default" means the pod subnet in
// one slot and the service subnet in the other, so sharing one registry would
// make the name ambiguous.
var (
	projectDrivers = newRegistry[ProjectDriver]("project")
	subnetDrivers  = newRegistry[SubnetDriver]("subnet")
	sgDrivers      = newRegistry[SecurityGroupsDriver]("security groups")
	podVIFDrivers  = newRegistry[PodVIFDriver]("pod VIF")

	serviceSubnetDrivers = newRegistry[SubnetDriver]("service subnet")
	serviceSGDrivers     = newRegistry[SecurityGroupsDriver]("service security groups")
)

// RegisterProjectDriver adds a project driver under a name.
func RegisterProjectDriver(name string, f func(Deps) (ProjectDriver, error)) {
	projectDrivers.register(name, f)
}

// RegisterSubnetDriver adds a pod subnet driver under a name.
func RegisterSubnetDriver(name string, f func(Deps) (SubnetDriver, error)) {
	subnetDrivers.register(name, f)
}

// RegisterSecurityGroupsDriver adds a pod security groups driver under a name.
func RegisterSecurityGroupsDriver(name string, f func(Deps) (SecurityGroupsDriver, error)) {
	sgDrivers.register(name, f)
}

// RegisterPodVIFDriver adds a pod VIF driver under a name.
func RegisterPodVIFDriver(name string, f func(Deps) (PodVIFDriver, error)) {
	podVIFDrivers.register(name, f)
}

// RegisterServiceSubnetDriver adds a service subnet driver under a name.
func RegisterServiceSubnetDriver(name string, f func(Deps) (SubnetDriver, error)) {
	serviceSubnetDrivers.register(name, f)
}

// RegisterServiceSecurityGroupsDriver adds a service security groups driver
// under a name.
func RegisterServiceSecurityGroupsDriver(name string, f func(Deps) (SecurityGroupsDriver, error)) {
	serviceSGDrivers.register(name, f)
}

// NewProjectDriver builds the named project driver.
func NewProjectDriver(name string, deps Deps) (ProjectDriver, error) {
	return projectDrivers.build(name, deps)
}

// NewSubnetDriver builds the named pod subnet driver.
func NewSubnetDriver(name string, deps Deps) (SubnetDriver, error) {
	return subnetDrivers.build(name, deps)
}

// NewSecurityGroupsDriver builds the named pod security groups driver.
func NewSecurityGroupsDriver(name string, deps Deps) (SecurityGroupsDriver, error) {
	return sgDrivers.build(name, deps)
}

// NewPodVIFDriver builds the named pod VIF driver.
func NewPodVIFDriver(name string, deps Deps) (PodVIFDriver, error) {
	return podVIFDrivers.build(name, deps)
}

// NewServiceSubnetDriver builds the named service subnet driver.
func NewServiceSubnetDriver(name string, deps Deps) (SubnetDriver, error) {
	return serviceSubnetDrivers.build(name, deps)
}

// NewServiceSecurityGroupsDriver builds the named service security groups
// driver.
func NewServiceSecurityGroupsDriver(name string, deps Deps) (SecurityGroupsDriver, error) {
	return serviceSGDrivers.build(name, deps)
}

// unknownDriver reports the available names alongside the bad one. A
// misconfigured driver name is one of the easiest mistakes to make in
// kuryr.conf and one of the most confusing to debug without them.
func unknownDriver(kind, name string, available []string) error {
	return fmt.Errorf("unknown %s driver %q; available: %v", kind, name, available)
}
