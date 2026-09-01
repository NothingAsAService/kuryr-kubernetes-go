package controller

// The permissions below are needed by the controller process but are not
// requested by any one reconciler, so there is nowhere natural to put the
// markers. Keeping them here, together and explained, is better than a role
// that is hand-edited after every generation.

// Leader election. The manager holds a Lease so exactly one replica is
// active; two controllers reconciling the same pod would both create Neutron
// ports and only one would win the status write, leaking the other.
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Nodes are read by the multi-pool driver, which picks a pool from the
// pod_vif label on the node a pod landed on.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// NetworkAttachmentDefinitions are read by the NPWG multi-VIF driver to find
// out which subnet and driver an extra interface should use. Read-only: the
// definitions belong to Multus, Kuryr only consults them.
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch
