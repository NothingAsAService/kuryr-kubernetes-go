package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Finalizers Kuryr places on objects so that OpenStack resources are released
// before Kubernetes drops the object.
const (
	// PodFinalizer is set on a Pod and cleared once its ports are released.
	PodFinalizer = "openstack.org/pod-finalizer"
	// KuryrPortFinalizer is set on a KuryrPort and cleared last of all.
	KuryrPortFinalizer = "openstack.org/kuryrport-finalizer"
)

// NodeNameLabel carries the node a KuryrPort belongs to. The CNI daemon
// watches with a label selector on it, so each node only streams the
// KuryrPorts it will actually be asked to plug.
const NodeNameLabel = "kuryr.openstack.org/nodeName"

// DefaultIfName is the interface that receives the pod default route.
const DefaultIfName = "eth0"

// KuryrPortSpec is the immutable description of the pod a KuryrPort serves.
type KuryrPortSpec struct {
	// PodUID pins this KuryrPort to one incarnation of the pod. A StatefulSet
	// pod keeps its name across restarts but gets a fresh UID, so name alone
	// cannot tell a live pod from a dead one.
	PodUID types.UID `json:"podUid"`
	// PodNodeName is the node the pod was scheduled to.
	PodNodeName string `json:"podNodeName"`
	// PodStatic marks a static (mirror) pod. Static pods report a different
	// UID through the API than the one the CRI passes to CNI, so the UID
	// check has to be skipped for them.
	PodStatic bool `json:"podStatic,omitempty"`
}

// PortVIF is one interface attached to a pod.
type PortVIF struct {
	// Default marks the interface that carries the pod default route.
	Default bool `json:"default"`
	// VIF is the binding information for this interface.
	VIF VIF `json:"vif"`
}

// Condition types reported on a KuryrPort.
const (
	// KuryrPortConditionProvisioned is true once every VIF has a Neutron port.
	KuryrPortConditionProvisioned = "Provisioned"
	// KuryrPortConditionActive is true once every Neutron port is ACTIVE.
	KuryrPortConditionActive = "Active"
)

// KuryrPortStatus is the OpenStack side of the pod, as it actually exists.
type KuryrPortStatus struct {
	// VIFs maps interface name to its binding information. The Python
	// implementation stored serialized oslo.versionedobjects here; this is a
	// typed structure that the API server can validate.
	// +optional
	VIFs map[string]PortVIF `json:"vifs,omitempty"`

	// SecurityGroups are the groups currently applied to the ports. Recording
	// them is what lets the controller tell a real policy change from a
	// reconcile that changed nothing, so a running pod's ports are only
	// rewritten when the groups actually differ.
	// +optional
	SecurityGroups []string `json:"securityGroups,omitempty"`

	// Conditions reports provisioning progress. The Python implementation
	// leaned on Kubernetes Events for this, which are lossy and expire.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AllActive reports whether every VIF has reached ACTIVE.
func (s *KuryrPortStatus) AllActive() bool {
	if len(s.VIFs) == 0 {
		return false
	}
	for _, pv := range s.VIFs {
		if !pv.VIF.Active {
			return false
		}
	}
	return true
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=kuryrports,singular=kuryrport,shortName=kp,scope=Namespaced
// +kubebuilder:printcolumn:name="PodUID",type=string,JSONPath=`.spec.podUid`,description="Pod UID"
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.podNodeName`,description="Node the pod runs on"
// +kubebuilder:printcolumn:name="Active",type=string,JSONPath=`.status.conditions[?(@.type=="Active")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KuryrPort is the Neutron side of a Kubernetes pod. It is created by the pod
// controller and reconciled by the kuryrport controller, and it is the only
// channel through which the controller talks to the CNI daemon.
type KuryrPort struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KuryrPortSpec   `json:"spec,omitempty"`
	Status KuryrPortStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KuryrPortList is a list of KuryrPort.
type KuryrPortList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KuryrPort `json:"items"`
}

func init() {
	registerTypes(&KuryrPort{}, &KuryrPortList{})
}
