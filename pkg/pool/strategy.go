package pool

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

// strategy is the small part of pooling that genuinely differs between a
// baremetal deployment and a nested one.
//
// The Python implementation expressed this difference as two full subclasses
// of BaseVIFPool, which meant recycling, security group handling and pool
// bookkeeping were all written twice. Nearly all of that is identical; only
// these four things are not.
type strategy interface {
	// Name identifies the strategy in logs.
	Name() string

	// HostKey is the pool dimension that stands for "the machine this pod
	// runs on". For baremetal that is the node name, because the port binds
	// to the node. For nested it is the node VM address, because that is what
	// identifies the trunk the subport hangs off.
	HostKey(req drivers.PodVIFRequest) (string, error)

	// MarkInUse renames a port as it leaves the pool, when port_debug is on.
	MarkInUse(ctx context.Context, portID string, pod *corev1.Pod) error

	// MarkAvailable renames a port as it returns to the pool.
	MarkAvailable(ctx context.Context, portID string) error

	// UsesTrunks reports whether pooled ports are trunk subports, which
	// changes both their device_owner and how they are rediscovered.
	UsesTrunks() bool

	// Recover rebuilds the pools from ports left behind by a previous run.
	Recover(ctx context.Context, p *Pool) error
}

// strategyFor picks the strategy that matches a VIF driver.
func strategyFor(driverName string, os cloud, k8s client.Client, cfg *config.Config) (strategy, error) {
	base := baseStrategy{os: os, k8s: k8s, cfg: cfg}
	switch driverName {
	case "neutron-vif":
		return &baremetalStrategy{baseStrategy: base}, nil
	case "nested-vlan", "nested-macvlan":
		return &nestedStrategy{baseStrategy: base}, nil
	default:
		return nil, fmt.Errorf(
			"pooling is not supported for pod VIF driver %q; set [kubernetes]vif_pool_driver=noop",
			driverName)
	}
}

type baseStrategy struct {
	os  cloud
	k8s client.Client
	cfg *config.Config
}

// --- baremetal ---------------------------------------------------------

type baremetalStrategy struct{ baseStrategy }

// Name is the registered strategy name, used in logs and metrics.
func (s *baremetalStrategy) Name() string { return "baremetal" }

// UsesTrunks is false: a bare metal node plugs ports onto the integration
// bridge directly, with no trunk in the path.
func (s *baremetalStrategy) UsesTrunks() bool { return false }

// HostKey pools by node name. A bare metal port is bound to the node it was
// created for, so a port free on one node is of no use on another.
func (s *baremetalStrategy) HostKey(req drivers.PodVIFRequest) (string, error) {
	if req.HostID != "" {
		return req.HostID, nil
	}
	pod := podFor(req)
	if pod == nil || pod.Spec.NodeName == "" {
		return "", fmt.Errorf("pool: pod is not scheduled, so it has no node to pool against")
	}
	return pod.Spec.NodeName, nil
}

// MarkInUse names the port after the pod and points device_id at it, which is
// what makes a Neutron port list readable during an incident.
func (s *baremetalStrategy) MarkInUse(ctx context.Context, portID string, pod *corev1.Pod) error {
	name := pod.Namespace + "/" + pod.Name
	uid := string(pod.UID)
	_, err := s.os.UpdatePort(ctx, portID, openstack.PortUpdate{Name: &name, DeviceID: &uid})
	return err
}

// MarkAvailable undoes MarkInUse, returning the port to the pool name and
// clearing device_id so nothing appears to own it.
func (s *baremetalStrategy) MarkAvailable(ctx context.Context, portID string) error {
	name := openstack.PoolPortName
	empty := ""
	_, err := s.os.UpdatePort(ctx, portID, openstack.PortUpdate{Name: &name, DeviceID: &empty})
	return err
}

// Recover rebuilds the pool after a restart by listing the ports this node
// left behind.
func (s *baremetalStrategy) Recover(ctx context.Context, p *Pool) error {
	return recoverBaremetal(ctx, p)
}

// --- nested ------------------------------------------------------------

type nestedStrategy struct{ baseStrategy }

// Name is the registered strategy name, used in logs and metrics.
func (s *nestedStrategy) Name() string { return "nested" }

// UsesTrunks is true: a pod port on a nested deployment is a subport of the
// node VM's trunk, which is what the pool has to walk to find its ports again.
func (s *nestedStrategy) UsesTrunks() bool { return true }

// HostKey pools by the node VM's address, which is what identifies its trunk.
// The node name is no use here: the trunk belongs to the VM, not to the
// Kubernetes object describing it.
func (s *nestedStrategy) HostKey(req drivers.PodVIFRequest) (string, error) {
	if req.HostID != "" {
		return req.HostID, nil
	}
	pod := podFor(req)
	if pod == nil || pod.Status.HostIP == "" {
		return "", fmt.Errorf("pool: pod has no hostIP yet, so its node VM trunk cannot be identified")
	}
	return pod.Status.HostIP, nil
}

// MarkInUse sets only the name. A subport's device_id is not ours to set:
// Neutron manages it as part of the trunk attachment.
func (s *nestedStrategy) MarkInUse(ctx context.Context, portID string, pod *corev1.Pod) error {
	name := pod.Namespace + "/" + pod.Name
	_, err := s.os.UpdatePort(ctx, portID, openstack.PortUpdate{Name: &name})
	return err
}

// MarkAvailable renames the subport back to the pool name. There is no
// device_id to clear, for the reason MarkInUse gives.
func (s *nestedStrategy) MarkAvailable(ctx context.Context, portID string) error {
	name := openstack.PoolPortName
	_, err := s.os.UpdatePort(ctx, portID, openstack.PortUpdate{Name: &name})
	return err
}

// Recover rebuilds the pool after a restart by walking the node VM's trunk for
// subports the previous process left behind.
func (s *nestedStrategy) Recover(ctx context.Context, p *Pool) error {
	return recoverNested(ctx, p)
}
