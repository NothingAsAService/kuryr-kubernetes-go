package pool

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
)

// NodeVIFLabel selects which pod VIF driver a node uses. It keeps the name
// the Python implementation used so an existing cluster does not have to be
// relabelled.
const NodeVIFLabel = "pod_vif"

// Manager is the pooling surface the controller wires up: a VIF driver that
// also needs recovering at startup and recycling in the background.
type Manager interface {
	drivers.PodVIFDriver

	// Recover rebuilds pools from ports left by a previous run. Nothing is
	// served until it completes.
	Recover(ctx context.Context) error
	// RunRecycler blocks, returning released ports to their pools.
	RunRecycler(ctx context.Context) error
	// DeleteNetworkPools empties the pools on a network being torn down.
	DeleteNetworkPools(ctx context.Context, networkID string) error
	// NeedLeaderElection keeps port creation to the active controller.
	NeedLeaderElection() bool
}

var (
	_ Manager = (*Pool)(nil)
	_ Manager = (*Multi)(nil)
)

// Multi routes each pod to the pool matching its node.
//
// A cluster can be genuinely mixed: some nodes bare metal, some node VMs with
// trunks. The two need different pool strategies and different VIF drivers,
// and the node's own label is what says which.
type Multi struct {
	k8s          client.Client
	defaultAlias string
	pools        map[string]*Pool

	// nodeAlias memoises the node lookup. Node labels change about as often
	// as nodes are rebuilt, and doing this per pod would put a Get on the
	// hot path of every pod creation.
	nodeAlias sync.Map // node name -> alias
}

// NewMulti builds a pool per configured pod VIF driver.
//
// The mapping comes from [vif_pool]vif_pool_mapping. A cluster with a single
// driver does not need it and should use New directly.
func NewMulti(cfg *config.Config, os cloud, k8s client.Client,
	inner map[string]drivers.PodVIFDriver, metrics *Metrics) (*Multi, error) {

	if len(inner) == 0 {
		return nil, fmt.Errorf("multi pool: no pod VIF drivers configured")
	}

	m := &Multi{
		k8s:          k8s,
		defaultAlias: cfg.Kubernetes.PodVIFDriver,
		pools:        make(map[string]*Pool, len(inner)),
	}

	for alias, drv := range inner {
		p, err := New(cfg, os, k8s, drv, metrics)
		if err != nil {
			return nil, fmt.Errorf("multi pool: %w", err)
		}
		m.pools[alias] = p
	}

	if _, ok := m.pools[m.defaultAlias]; !ok {
		return nil, fmt.Errorf(
			"multi pool: [kubernetes]pod_vif_driver is %q but no pool was built for it; known: %v",
			m.defaultAlias, m.aliases())
	}
	return m, nil
}

func (m *Multi) aliases() []string {
	return slices.Sorted(maps.Keys(m.pools))
}

// Name reports the aliases behind this dispatcher.
func (m *Multi) Name() string { return fmt.Sprintf("multi%v", m.aliases()) }

// NeedLeaderElection keeps port creation on the active controller.
func (m *Multi) NeedLeaderElection() bool { return true }

// poolForPod picks the pool for the node a pod is scheduled to.
func (m *Multi) poolForPod(ctx context.Context, pod *corev1.Pod) (*Pool, error) {
	if pod == nil || pod.Spec.NodeName == "" {
		return nil, fmt.Errorf("multi pool: pod is not scheduled, so its node driver is unknown")
	}

	alias, err := m.aliasForNode(ctx, pod.Spec.NodeName)
	if err != nil {
		return nil, err
	}
	p, ok := m.pools[alias]
	if !ok {
		return nil, fmt.Errorf(
			"node %s is labelled %s=%s but no pool is configured for that driver; known: %v",
			pod.Spec.NodeName, NodeVIFLabel, alias, m.aliases())
	}
	return p, nil
}

func (m *Multi) aliasForNode(ctx context.Context, nodeName string) (string, error) {
	if cached, ok := m.nodeAlias.Load(nodeName); ok {
		return cached.(string), nil
	}

	var node corev1.Node
	if err := m.k8s.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return "", fmt.Errorf("reading node %s to pick a VIF driver: %w", nodeName, err)
	}

	alias := m.defaultAlias
	if v, ok := node.Labels[NodeVIFLabel]; ok && v != "" {
		alias = v
	}
	m.nodeAlias.Store(nodeName, alias)
	return alias, nil
}

// poolForVIF picks the pool from a VIF that already exists, which is what
// release and activate have to work from: the pod may be long gone.
func (m *Multi) poolForVIF(v *kuryrv1.VIF) (*Pool, error) {
	alias, ok := aliasForVIFType(v.Type)
	if !ok {
		return nil, fmt.Errorf("multi pool: no driver handles VIF type %q", v.Type)
	}
	p, ok := m.pools[alias]
	if !ok {
		return nil, fmt.Errorf("multi pool: no pool configured for driver %q (VIF type %q)", alias, v.Type)
	}
	return p, nil
}

// aliasForVIFType maps a VIF back to the driver that made it.
func aliasForVIFType(t kuryrv1.VIFType) (string, bool) {
	switch t {
	case kuryrv1.VIFTypeOVS, kuryrv1.VIFTypeBridge, kuryrv1.VIFTypeVHostUser:
		return "neutron-vif", true
	case kuryrv1.VIFTypeNestedVLAN:
		return "nested-vlan", true
	case kuryrv1.VIFTypeNestedMacvlan:
		return "nested-macvlan", true
	case kuryrv1.VIFTypeNestedDPDK:
		return "nested-dpdk", true
	default:
		return "", false
	}
}

// RequestVIF serves from the pool matching the pod's node.
func (m *Multi) RequestVIF(ctx context.Context, req drivers.PodVIFRequest) (*kuryrv1.VIF, error) {
	p, err := m.poolForPod(ctx, req.Pod)
	if err != nil {
		return nil, err
	}
	return p.RequestVIF(ctx, req)
}

// RequestVIFs is used for pool filling and follows the pod's node too.
func (m *Multi) RequestVIFs(ctx context.Context, req drivers.PodVIFRequest, count int) ([]*kuryrv1.VIF, error) {
	p, err := m.poolForPod(ctx, req.Pod)
	if err != nil {
		return nil, err
	}
	return p.RequestVIFs(ctx, req, count)
}

// ReleaseVIF routes on the VIF type, since the pod may already be gone.
func (m *Multi) ReleaseVIF(ctx context.Context, v *kuryrv1.VIF, req drivers.PodVIFRequest) error {
	p, err := m.poolForVIF(v)
	if err != nil {
		return err
	}
	return p.ReleaseVIF(ctx, v, req)
}

// ActivateVIF routes on the VIF type.
func (m *Multi) ActivateVIF(ctx context.Context, v *kuryrv1.VIF, req drivers.PodVIFRequest) error {
	p, err := m.poolForVIF(v)
	if err != nil {
		return err
	}
	return p.ActivateVIF(ctx, v, req)
}

// UpdateVIFSecurityGroups routes on the VIF type.
func (m *Multi) UpdateVIFSecurityGroups(ctx context.Context, v *kuryrv1.VIF, sgs []string) error {
	p, err := m.poolForVIF(v)
	if err != nil {
		return err
	}
	return p.UpdateVIFSecurityGroups(ctx, v, sgs)
}

// Recover recovers every pool. A failure in one is fatal: serving pods from a
// partially recovered set would create duplicate ports on that driver.
func (m *Multi) Recover(ctx context.Context) error {
	var errs []error
	for alias, p := range m.pools {
		if err := p.Recover(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", alias, err))
		}
	}
	return errors.Join(errs...)
}

// RunRecycler runs every pool's recycler until the context is cancelled.
func (m *Multi) RunRecycler(ctx context.Context) error {
	var wg sync.WaitGroup
	for alias, p := range m.pools {
		wg.Add(1)
		go func(alias string, p *Pool) {
			defer wg.Done()
			if err := p.RunRecycler(ctx); err != nil {
				log.FromContext(ctx).Error(err, "pool recycler stopped", "driver", alias)
			}
		}(alias, p)
	}
	wg.Wait()
	return nil
}

// DeleteNetworkPools clears a network from every pool.
func (m *Multi) DeleteNetworkPools(ctx context.Context, networkID string) error {
	var errs []error
	for alias, p := range m.pools {
		if err := p.DeleteNetworkPools(ctx, networkID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", alias, err))
		}
	}
	return errors.Join(errs...)
}

// ForgetNode drops the cached driver alias for a node, for use when a node is
// relabelled or replaced.
func (m *Multi) ForgetNode(nodeName string) { m.nodeAlias.Delete(nodeName) }

// Stats merges the per-pool occupancy.
func (m *Multi) Stats() map[string]int {
	out := map[string]int{}
	for alias, p := range m.pools {
		for k, v := range p.Stats() {
			out[alias+"|"+k] = v
		}
	}
	return out
}
