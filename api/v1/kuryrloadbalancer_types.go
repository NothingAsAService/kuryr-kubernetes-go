package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KuryrLoadBalancerFinalizer guards Octavia teardown.
const KuryrLoadBalancerFinalizer = "openstack.org/kuryrloadbalancer-finalizer"

// ServicePort is one port exposed by a Service.
type ServicePort struct {
	// Name is the port name, empty for single-port services.
	// +optional
	Name string `json:"name,omitempty"`
	// Protocol is TCP, UDP or SCTP.
	Protocol corev1.Protocol `json:"protocol"`
	// Port is the port the load balancer listens on.
	Port int32 `json:"port"`
	// TargetPort is the backend port, possibly a named port.
	TargetPort string `json:"targetPort"`
}

// EndpointAddress is one backend behind a service port.
type EndpointAddress struct {
	// IP is the backend address.
	IP string `json:"ip"`
	// Ready reports whether the backend is currently serving.
	Ready bool `json:"ready"`
	// TargetRef points at the pod, when there is one.
	// +optional
	TargetRef *corev1.ObjectReference `json:"targetRef,omitempty"`
}

// EndpointSubset groups backends with the ports they serve.
type EndpointSubset struct {
	// Addresses are the backends.
	// +optional
	Addresses []EndpointAddress `json:"addresses,omitempty"`
	// Ports are the ports those backends serve.
	// +optional
	Ports []ServicePort `json:"ports,omitempty"`
}

// KuryrLoadBalancerSpec merges what the Service says with what the
// EndpointSlices say. Either controller may write first; the load balancer
// controller waits until both halves are present.
type KuryrLoadBalancerSpec struct {
	// ProjectID is the OpenStack project to build in.
	// +optional
	ProjectID string `json:"projectID,omitempty"`
	// SubnetID is the subnet the load balancer VIP lives on.
	// +optional
	SubnetID string `json:"subnetID,omitempty"`
	// IP is the Service ClusterIP, used as the VIP.
	// +optional
	IP string `json:"ip,omitempty"`
	// LoadBalancerIP is the requested external address, if any.
	// +optional
	LoadBalancerIP string `json:"lbIP,omitempty"`
	// Type mirrors Service.spec.type.
	// +optional
	Type string `json:"type,omitempty"`
	// Provider selects the Octavia provider driver.
	// +optional
	Provider string `json:"provider,omitempty"`
	// Ports are the service ports to expose.
	// +optional
	Ports []ServicePort `json:"ports,omitempty"`
	// SecurityGroupIDs are the groups applied to the VIP port.
	// +optional
	SecurityGroupIDs []string `json:"securityGroupIDs,omitempty"`
	// Subsets carries the backends collected from EndpointSlices.
	// +optional
	Subsets []EndpointSubset `json:"subsets,omitempty"`
	// TimeoutClientData is the frontend inactivity timeout in milliseconds.
	// +optional
	TimeoutClientData int32 `json:"timeoutClientData,omitempty"`
	// TimeoutMemberData is the backend inactivity timeout in milliseconds.
	// +optional
	TimeoutMemberData int32 `json:"timeoutMemberData,omitempty"`
}

// HasBackends reports whether any endpoints have been recorded yet.
func (s *KuryrLoadBalancerSpec) HasBackends() bool {
	for i := range s.Subsets {
		if len(s.Subsets[i].Addresses) > 0 {
			return true
		}
	}
	return false
}

// LoadBalancer is the Octavia load balancer backing a Service.
type LoadBalancer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"projectID"`
	SubnetID  string `json:"subnetID"`
	IP        string `json:"ip"`
	PortID    string `json:"portID"`
	Provider  string `json:"provider,omitempty"`
	// +optional
	SecurityGroups []string `json:"securityGroups,omitempty"`
}

// Listener is an Octavia listener.
type Listener struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	ProjectID      string          `json:"projectID"`
	LoadBalancerID string          `json:"loadbalancerID"`
	Protocol       corev1.Protocol `json:"protocol"`
	Port           int32           `json:"port"`
	// +optional
	TimeoutClientData int32 `json:"timeoutClientData,omitempty"`
	// +optional
	TimeoutMemberData int32 `json:"timeoutMemberData,omitempty"`
}

// Pool is an Octavia pool.
type Pool struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	ProjectID      string          `json:"projectID"`
	LoadBalancerID string          `json:"loadbalancerID"`
	ListenerID     string          `json:"listenerID"`
	Protocol       corev1.Protocol `json:"protocol"`
}

// Member is an Octavia pool member, one backend pod.
type Member struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"projectID"`
	PoolID    string `json:"poolID"`
	SubnetID  string `json:"subnetID"`
	IP        string `json:"ip"`
	Port      int32  `json:"port"`
}

// PublicIPInfo records an allocated floating IP so it can be released.
type PublicIPInfo struct {
	IPID        string `json:"ipID"`
	IPAddr      string `json:"ipAddr"`
	AllocMethod string `json:"allocMethod"`
}

// Condition types reported on a KuryrLoadBalancer.
const (
	// KuryrLoadBalancerConditionReady is true once the load balancer,
	// listeners, pools and members are all in sync with the spec.
	KuryrLoadBalancerConditionReady = "Ready"
)

// KuryrLoadBalancerStatus is the Octavia topology as it actually exists.
type KuryrLoadBalancerStatus struct {
	// +optional
	LoadBalancer *LoadBalancer `json:"loadbalancer,omitempty"`
	// +optional
	Listeners []Listener `json:"listeners,omitempty"`
	// +optional
	Pools []Pool `json:"pools,omitempty"`
	// +optional
	Members []Member `json:"members,omitempty"`
	// +optional
	PublicIP *PublicIPInfo `json:"servicePubIPInfo,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=kuryrloadbalancers,singular=kuryrloadbalancer,shortName=klb,scope=Namespaced
// +kubebuilder:printcolumn:name="VIP",type=string,JSONPath=`.status.loadbalancer.ip`,description="Load balancer VIP"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KuryrLoadBalancer is the Octavia side of a Kubernetes Service.
type KuryrLoadBalancer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KuryrLoadBalancerSpec   `json:"spec,omitempty"`
	Status KuryrLoadBalancerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KuryrLoadBalancerList is a list of KuryrLoadBalancer.
type KuryrLoadBalancerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KuryrLoadBalancer `json:"items"`
}

func init() {
	registerTypes(&KuryrLoadBalancer{}, &KuryrLoadBalancerList{})
}
