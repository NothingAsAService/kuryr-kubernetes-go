// Package v1 contains the API types Kuryr stores in Kubernetes.
//
// This is the Go rewrite of the CRDs that kuryr-kubernetes (Python) defined in
// kubernetes_crds/kuryr_crds/. The group and version are kept identical
// (openstack.org/v1) so existing tooling, RBAC and kubectl muscle memory keep
// working, but the payloads are native Go structs rather than serialized
// oslo.versionedobjects blobs.
//
// The scheme wiring deliberately depends on apimachinery alone. An API
// package is imported by everything that talks to these types -- controllers,
// the CNI daemon, other people's tooling -- so pulling controller-runtime in
// here would put the whole manager machinery on their dependency graph for
// nothing.
//
// +kubebuilder:object:generate=true
// +groupName=openstack.org
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group/version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: "openstack.org", Version: "v1"}

// SchemeBuilder collects the registrations the types files add from their init
// functions. The seed registers the group-version's own metadata types, which
// is what makes List options and the like decode.
var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
})

// AddToScheme adds the types in this group-version to the given Scheme.
var AddToScheme = SchemeBuilder.AddToScheme

// registerTypes adds objects to the scheme under this group-version.
//
// Each types file calls it from an init function so a type's registration sits
// next to its definition rather than in one list that is easy to forget to
// update.
func registerTypes(objs ...runtime.Object) {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, objs...)
		return nil
	})
}
