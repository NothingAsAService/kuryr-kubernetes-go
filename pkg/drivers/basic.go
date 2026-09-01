package drivers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

// ProjectAnnotation lets an operator pin a namespace, and everything in it,
// to a particular OpenStack project.
const ProjectAnnotation = "openstack.org/kuryr-project"

func init() {
	RegisterProjectDriver("default", newDefaultProjectDriver)
	RegisterProjectDriver("annotation", newAnnotationProjectDriver)
	RegisterSubnetDriver("default", newDefaultSubnetDriver)
	RegisterSubnetDriver("namespace", newNamespaceSubnetDriver)
	RegisterSecurityGroupsDriver("default", newDefaultSGDriver)
}

// --- project drivers ---------------------------------------------------

// defaultProjectDriver puts everything in one project, named in kuryr.conf.
type defaultProjectDriver struct{ projectID string }

func newDefaultProjectDriver(d Deps) (ProjectDriver, error) {
	if d.Config.NeutronDefaults.Project == "" {
		return nil, fmt.Errorf("[neutron_defaults]project is required by the default project driver")
	}
	return &defaultProjectDriver{projectID: d.Config.NeutronDefaults.Project}, nil
}

// ProjectFor returns the one configured project, whatever the object is.
func (p *defaultProjectDriver) ProjectFor(context.Context, client.Object) (string, error) {
	return p.projectID, nil
}

// annotationProjectDriver reads the project from an annotation on the
// object's namespace, falling back to the configured default.
type annotationProjectDriver struct {
	k8s       client.Client
	projectID string
}

func newAnnotationProjectDriver(d Deps) (ProjectDriver, error) {
	return &annotationProjectDriver{k8s: d.K8s, projectID: d.Config.NeutronDefaults.Project}, nil
}

// ProjectFor reads the project from the object's namespace annotation, falling
// back to the configured default when the namespace does not pin one.
func (p *annotationProjectDriver) ProjectFor(ctx context.Context, obj client.Object) (string, error) {
	// A Namespace carries the annotation directly; anything else inherits it
	// from the namespace it lives in.
	if annotations := obj.GetAnnotations(); annotations != nil {
		if v, ok := annotations[ProjectAnnotation]; ok && v != "" {
			return v, nil
		}
	}

	nsName := obj.GetNamespace()
	if nsName == "" {
		if p.projectID == "" {
			return "", fmt.Errorf("no project annotation on %s and no default configured", obj.GetName())
		}
		return p.projectID, nil
	}

	var ns corev1.Namespace
	if err := p.k8s.Get(ctx, types.NamespacedName{Name: nsName}, &ns); err != nil {
		return "", fmt.Errorf("reading namespace %s for project annotation: %w", nsName, err)
	}
	if v, ok := ns.Annotations[ProjectAnnotation]; ok && v != "" {
		return v, nil
	}
	if p.projectID == "" {
		return "", fmt.Errorf("namespace %s has no %s annotation and no default project is configured",
			nsName, ProjectAnnotation)
	}
	return p.projectID, nil
}

// --- subnet drivers ----------------------------------------------------

// defaultSubnetDriver hands every pod the same subnet from kuryr.conf.
type defaultSubnetDriver struct {
	os       *openstack.Client
	subnetID string
}

func newDefaultSubnetDriver(d Deps) (SubnetDriver, error) {
	if d.Config.NeutronDefaults.PodSubnet == "" {
		return nil, fmt.Errorf("[neutron_defaults]pod_subnet is required by the default subnet driver")
	}
	return &defaultSubnetDriver{os: d.OpenStack, subnetID: d.Config.NeutronDefaults.PodSubnet}, nil
}

// SubnetsFor returns the single pod subnet from kuryr.conf, the same one for
// every pod.
func (s *defaultSubnetDriver) SubnetsFor(ctx context.Context, _ client.Object,
	_ string) (vif.SubnetSet, error) {

	sub, err := s.os.GetSubnet(ctx, s.subnetID)
	if err != nil {
		return nil, err
	}
	return vif.SubnetSet{sub.ID: sub}, nil
}

// namespaceSubnetDriver gives each Kubernetes namespace its own subnet,
// discovered through the KuryrNetwork CRD the namespace controller maintains.
type namespaceSubnetDriver struct {
	os  *openstack.Client
	k8s client.Client
}

func newNamespaceSubnetDriver(d Deps) (SubnetDriver, error) {
	return &namespaceSubnetDriver{os: d.OpenStack, k8s: d.K8s}, nil
}

// ErrSubnetNotReady means the namespace network exists as an intent but its
// Neutron resources are not built yet. The caller requeues rather than fails.
type ErrSubnetNotReady struct{ Namespace string }

func (e *ErrSubnetNotReady) Unwrap() error { return ErrRetryLater }

func (e *ErrSubnetNotReady) Error() string {
	return fmt.Sprintf("subnet for namespace %s is not provisioned yet", e.Namespace)
}

// SubnetsFor returns the subnet the namespace controller provisioned for this
// object's namespace. It reports ErrSubnetNotReady, which the caller retries,
// while that subnet is still being built.
func (s *namespaceSubnetDriver) SubnetsFor(ctx context.Context, obj client.Object,
	_ string) (vif.SubnetSet, error) {

	nsName := obj.GetNamespace()
	if nsName == "" {
		nsName = obj.GetName()
	}

	var kns kuryrv1.KuryrNetwork
	key := types.NamespacedName{Namespace: nsName, Name: nsName}
	if err := s.k8s.Get(ctx, key, &kns); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// The namespace controller has not caught up yet.
			return nil, &ErrSubnetNotReady{Namespace: nsName}
		}
		return nil, fmt.Errorf("reading KuryrNetwork %s: %w", nsName, err)
	}
	if kns.Status.SubnetID == "" {
		return nil, &ErrSubnetNotReady{Namespace: nsName}
	}

	sub, err := s.os.GetSubnet(ctx, kns.Status.SubnetID)
	if err != nil {
		return nil, err
	}
	return vif.SubnetSet{sub.ID: sub}, nil
}

// --- security group drivers --------------------------------------------

// defaultSGDriver applies a fixed set of groups from kuryr.conf.
type defaultSGDriver struct{ groups []string }

func newDefaultSGDriver(d Deps) (SecurityGroupsDriver, error) {
	return &defaultSGDriver{groups: d.Config.NeutronDefaults.PodSecurityGroups}, nil
}

// SecurityGroupsFor returns the fixed set of groups from kuryr.conf.
func (s *defaultSGDriver) SecurityGroupsFor(context.Context, client.Object, string) ([]string, error) {
	// Copy so a caller cannot mutate the configured slice underneath us.
	return append([]string(nil), s.groups...), nil
}
