package drivers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

// SubnetAnnotation lets a workload pick the Neutron subnet its ports come
// from, instead of taking whatever the cluster-wide default is.
//
// It is read from the pod first and from the pod's namespace second, so a
// tenant can pin a whole namespace once and still let an individual pod opt
// into a different subnet of the same tenant.
const SubnetAnnotation = "openstack.org/kuryr-subnet-id"

func init() {
	RegisterSubnetDriver("annotation", newAnnotationSubnetDriver)
}

// ErrSubnetNotAllowed means a pod asked for a subnet that belongs to another
// project.
//
// It deliberately does not wrap ErrRetryLater. Waiting cannot fix it: the
// annotation has to change, or the namespace has to be moved to the project
// that owns the subnet. Retrying instead would turn a clear misconfiguration
// into a pod that hangs with no explanation.
type ErrSubnetNotAllowed struct {
	SubnetID string
	// SubnetProject owns the subnet; PodProject owns the pod. They differ,
	// which is the whole problem.
	SubnetProject string
	PodProject    string
}

func (e *ErrSubnetNotAllowed) Error() string {
	return fmt.Sprintf(
		"subnet %s belongs to project %s, but the pod belongs to project %s; "+
			"a pod may only ask for a subnet in its own project",
		e.SubnetID, orUnknown(e.SubnetProject), orUnknown(e.PodProject))
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// annotationSubnetDriver reads the subnet from an annotation, and delegates
// to another driver for everything that carries none.
//
// It is a decorator rather than a standalone driver so that annotating a pod
// works the same whether the cluster otherwise puts every pod on one subnet
// or gives each namespace its own: the annotation is an override, not a
// replacement for how the rest of the cluster gets addresses.
type annotationSubnetDriver struct {
	// os is narrowed to subnetReader, declared in multivif.go, so the
	// annotation handling can be tested without a cloud.
	os   subnetReader
	k8s  client.Client
	base SubnetDriver
	// enforceProject refuses a subnet owned by a project other than the pod's.
	// This is what keeps the annotation from being a way out of a tenant: with
	// it off, any user who can set a pod annotation can put a port on any
	// subnet the controller's credentials can see.
	enforceProject bool
}

func newAnnotationSubnetDriver(d Deps) (SubnetDriver, error) {
	fallback := d.Config.AnnotationSubnet.FallbackDriver
	if fallback == "" {
		fallback = "default"
	}
	if fallback == "annotation" {
		return nil, fmt.Errorf(
			"[annotation_subnet]fallback_driver cannot be annotation; it names the driver " +
				"that serves pods carrying no " + SubnetAnnotation + " annotation")
	}

	base, err := NewSubnetDriver(fallback, d)
	if err != nil {
		return nil, fmt.Errorf("building the fallback subnet driver for the annotation driver: %w", err)
	}

	return &annotationSubnetDriver{
		os:             d.OpenStack,
		k8s:            d.K8s,
		base:           base,
		enforceProject: d.Config.AnnotationSubnet.EnforceProject,
	}, nil
}

// SubnetsFor returns the annotated subnet, or defers to the fallback driver
// when nothing asked for one.
func (s *annotationSubnetDriver) SubnetsFor(ctx context.Context, obj client.Object,
	projectID string) (vif.SubnetSet, error) {

	subnetID, source, err := s.annotatedSubnet(ctx, obj)
	if err != nil {
		return nil, err
	}
	if subnetID == "" {
		return s.base.SubnetsFor(ctx, obj, projectID)
	}

	sub, err := s.os.GetSubnet(ctx, subnetID)
	if err != nil {
		// Name the annotation in the error: the subnet ID came from a user,
		// not from kuryr.conf, and the message is what tells them which one
		// of the two to go and fix.
		return nil, fmt.Errorf("%s on %s asks for subnet %s: %w",
			SubnetAnnotation, source, subnetID, err)
	}

	if s.enforceProject && sub.ProjectID != projectID {
		return nil, &ErrSubnetNotAllowed{
			SubnetID:      sub.ID,
			SubnetProject: sub.ProjectID,
			PodProject:    projectID,
		}
	}

	return vif.SubnetSet{sub.ID: sub}, nil
}

// annotatedSubnet finds the subnet an object asks for, and says where the
// request came from so an error can point at it.
func (s *annotationSubnetDriver) annotatedSubnet(ctx context.Context,
	obj client.Object) (subnetID, source string, err error) {

	if v := obj.GetAnnotations()[SubnetAnnotation]; v != "" {
		return v, describeObject(obj), nil
	}

	// A Namespace carries the annotation itself; anything else inherits it
	// from the namespace it lives in.
	nsName := obj.GetNamespace()
	if nsName == "" {
		return "", "", nil
	}

	var ns corev1.Namespace
	if err := s.k8s.Get(ctx, types.NamespacedName{Name: nsName}, &ns); err != nil {
		return "", "", fmt.Errorf("reading namespace %s for the %s annotation: %w",
			nsName, SubnetAnnotation, err)
	}
	if v := ns.Annotations[SubnetAnnotation]; v != "" {
		return v, "namespace " + nsName, nil
	}
	return "", "", nil
}

// describeObject names an object for an error message, without needing its
// Kubernetes kind.
func describeObject(obj client.Object) string {
	if ns := obj.GetNamespace(); ns != "" {
		return ns + "/" + obj.GetName()
	}
	return obj.GetName()
}
