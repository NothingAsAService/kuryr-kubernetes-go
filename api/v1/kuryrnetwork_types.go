package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KuryrNetworkFinalizer guards namespace network teardown.
const KuryrNetworkFinalizer = "openstack.org/kuryrnetwork-finalizer"

// KuryrNetworkSpec describes the namespace a KuryrNetwork serves.
type KuryrNetworkSpec struct {
	// NamespaceName is the Kubernetes namespace this network belongs to.
	NamespaceName string `json:"nsName"`
	// ProjectID is the OpenStack project the resources are created in.
	ProjectID string `json:"projectId"`
	// NamespaceLabels mirrors the namespace labels. NetworkPolicy rules can
	// select on them, so a change here has to trigger SG recalculation.
	// +optional
	NamespaceLabels map[string]string `json:"nsLabels,omitempty"`
}

// Condition types reported on a KuryrNetwork.
const (
	// KuryrNetworkConditionReady is true once network, subnet and router
	// interface all exist.
	KuryrNetworkConditionReady = "Ready"
)

// KuryrNetworkStatus accumulates the OpenStack resources as they are created.
//
// Each field is written as soon as its resource exists, so a controller that
// dies mid-sequence resumes at the right step rather than starting over.
type KuryrNetworkStatus struct {
	// NetID is the Neutron network UUID.
	// +optional
	NetID string `json:"netId,omitempty"`
	// SubnetID is the Neutron subnet UUID.
	// +optional
	SubnetID string `json:"subnetId,omitempty"`
	// SubnetCIDR is the range allocated from the subnet pool.
	// +optional
	SubnetCIDR string `json:"subnetCIDR,omitempty"`
	// RouterID is the router the subnet was attached to.
	// +optional
	RouterID string `json:"routerId,omitempty"`
	// Populated records that the port pools for this subnet were primed.
	// It is set before priming rather than after: a failed prime is cheap to
	// retry, whereas priming twice leaks ports.
	// +optional
	Populated bool `json:"populated,omitempty"`
	// NamespaceLabels is the last label set the security groups were
	// reconciled against.
	// +optional
	NamespaceLabels map[string]string `json:"nsLabels,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=kuryrnetworks,singular=kuryrnetwork,shortName=kns,scope=Namespaced
// +kubebuilder:printcolumn:name="SUBNET-CIDR",type=string,JSONPath=`.status.subnetCIDR`,description="Subnet CIDR allocated to the namespace"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KuryrNetwork is the Neutron network backing one Kubernetes namespace.
type KuryrNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KuryrNetworkSpec   `json:"spec,omitempty"`
	Status KuryrNetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KuryrNetworkList is a list of KuryrNetwork.
type KuryrNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KuryrNetwork `json:"items"`
}

func init() {
	registerTypes(&KuryrNetwork{}, &KuryrNetworkList{})
}
