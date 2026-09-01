package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KuryrNetworkPolicyFinalizer guards security group teardown.
const KuryrNetworkPolicyFinalizer = "openstack.org/kuryrnetworkpolicy-finalizer"

// SGDirection is the direction a security group rule applies to.
// +kubebuilder:validation:Enum=ingress;egress
type SGDirection string

const (
	// SGDirectionIngress matches traffic entering the port.
	SGDirectionIngress SGDirection = "ingress"
	// SGDirectionEgress matches traffic leaving the port.
	SGDirectionEgress SGDirection = "egress"
)

// SGEtherType is the address family a security group rule applies to.
// +kubebuilder:validation:Enum=IPv4;IPv6
type SGEtherType string

const (
	// SGEtherTypeIPv4 selects IPv4 traffic.
	SGEtherTypeIPv4 SGEtherType = "IPv4"
	// SGEtherTypeIPv6 selects IPv6 traffic.
	SGEtherTypeIPv6 SGEtherType = "IPv6"
)

// SGRule is a Neutron security group rule in the shape Kuryr needs.
//
// The Python CRD used Neutron wire names here (port_range_max,
// remote_ip_prefix). Those are translated at the gophercloud boundary now, so
// the stored object follows Kubernetes camelCase like every other field.
type SGRule struct {
	// Direction is ingress or egress.
	Direction SGDirection `json:"direction"`
	// EtherType is IPv4 or IPv6.
	EtherType SGEtherType `json:"etherType"`
	// Protocol is tcp, udp, sctp or empty for any.
	// +optional
	Protocol string `json:"protocol,omitempty"`
	// PortRangeMin is the low end of the port range, zero for any.
	// +optional
	PortRangeMin int32 `json:"portRangeMin,omitempty"`
	// PortRangeMax is the high end of the port range, zero for any.
	// +optional
	PortRangeMax int32 `json:"portRangeMax,omitempty"`
	// RemoteIPPrefix restricts the rule to a CIDR.
	// +optional
	RemoteIPPrefix string `json:"remoteIPPrefix,omitempty"`
	// Description carries provenance so an operator reading Neutron can tell
	// which NetworkPolicy produced a rule.
	// +optional
	Description string `json:"description,omitempty"`
}

// AffectedPod is a pod a rule was expanded from.
type AffectedPod struct {
	// PodIP is the pod address the rule was generated for.
	PodIP string `json:"podIP"`
	// PodNamespace is the namespace of that pod.
	PodNamespace string `json:"podNamespace"`
}

// SGRuleSpec is one desired rule plus the pods it was derived from.
type SGRuleSpec struct {
	// Rule is the security group rule to create.
	Rule SGRule `json:"sgRule"`
	// AffectedPods lists the pods that made this rule necessary. Keeping the
	// derivation lets the controller recompute incrementally when a single
	// pod changes rather than rebuilding the whole group.
	// +optional
	AffectedPods []AffectedPod `json:"affectedPods,omitempty"`
	// Namespace scopes the rule when it came from a namespaceSelector.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// CreatedSGRule is a rule that exists in Neutron, with its assigned ID.
type CreatedSGRule struct {
	// ID is the Neutron security group rule UUID.
	ID string `json:"id"`
	// SecurityGroupID is the group the rule belongs to.
	// +optional
	SecurityGroupID string `json:"securityGroupID,omitempty"`
	// Rule is the rule content as created.
	Rule SGRule `json:"rule"`
}

// KuryrNetworkPolicySpec is the translated form of a Kubernetes
// NetworkPolicy, expressed entirely in security group terms.
type KuryrNetworkPolicySpec struct {
	// IngressRules are the rules for traffic entering selected pods.
	// +optional
	IngressRules []SGRuleSpec `json:"ingressSgRules,omitempty"`
	// EgressRules are the rules for traffic leaving selected pods.
	// +optional
	EgressRules []SGRuleSpec `json:"egressSgRules,omitempty"`
	// PodSelector selects the pods the group is applied to.
	PodSelector metav1.LabelSelector `json:"podSelector"`
	// PolicyTypes mirrors NetworkPolicy.spec.policyTypes.
	// +optional
	PolicyTypes []string `json:"policyTypes,omitempty"`
}

// Condition types reported on a KuryrNetworkPolicy.
const (
	// KuryrNetworkPolicyConditionEnforced is true once the security group and
	// all its rules exist and are applied to the selected ports.
	KuryrNetworkPolicyConditionEnforced = "Enforced"
)

// KuryrNetworkPolicyStatus is the security group as it exists in Neutron.
type KuryrNetworkPolicyStatus struct {
	// SecurityGroupID is the Neutron security group backing this policy.
	// +optional
	SecurityGroupID string `json:"securityGroupID,omitempty"`
	// SecurityGroupRules are the rules that were actually created. The
	// difference between this and the spec is the work still to be done.
	// +optional
	SecurityGroupRules []CreatedSGRule `json:"securityGroupRules,omitempty"`
	// PodSelector is the selector the current rules were computed from.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=kuryrnetworkpolicies,singular=kuryrnetworkpolicy,shortName=knp,scope=Namespaced
// +kubebuilder:printcolumn:name="SG-ID",type=string,JSONPath=`.status.securityGroupID`,description="Security group backing the policy"
// +kubebuilder:printcolumn:name="Enforced",type=string,JSONPath=`.status.conditions[?(@.type=="Enforced")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KuryrNetworkPolicy is a Kubernetes NetworkPolicy translated into a Neutron
// security group.
type KuryrNetworkPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KuryrNetworkPolicySpec   `json:"spec,omitempty"`
	Status KuryrNetworkPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KuryrNetworkPolicyList is a list of KuryrNetworkPolicy.
type KuryrNetworkPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KuryrNetworkPolicy `json:"items"`
}

func init() {
	registerTypes(&KuryrNetworkPolicy{}, &KuryrNetworkPolicyList{})
}
