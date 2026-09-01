package pool

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

// Recover rebuilds the pools from ports a previous controller left behind.
//
// Without this a restart would strand every pooled port: Neutron still has
// them, but nothing knows they exist, so the controller creates more and the
// project runs out of quota. This is why RequestVIF refuses to serve until
// recovery has finished.
func (p *Pool) Recover(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("pool-recovery")
	logger.Info("recovering pre-created ports", "strategy", p.strategy.Name())

	if err := p.strategy.Recover(ctx, p); err != nil {
		return fmt.Errorf("recovering ports pool: %w", err)
	}

	p.ready.Store(true)
	stats := p.Stats()
	logger.Info("ports pool ready", "pools", len(stats), "ports", totalOf(stats))
	for name, size := range stats {
		logger.V(1).Info("recovered pool", "pool", name, "ports", size)
	}
	return nil
}

func totalOf(stats map[string]int) int {
	n := 0
	for _, v := range stats {
		n += v
	}
	return n
}

// inUsePortIDs collects every port that a live pod already holds.
//
// The KuryrPort CRDs are the record of what is in use. Anything tagged as
// Kuryr's that is not in that set is either pooled or leaked, and either way
// belongs back under the controller's management.
func (p *Pool) inUsePortIDs(ctx context.Context) (map[string]bool, error) {
	var list kuryrv1.KuryrPortList
	if err := p.k8s.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing KuryrPorts to find in-use ports: %w", err)
	}

	inUse := map[string]bool{}
	for i := range list.Items {
		for _, pv := range list.Items[i].Status.VIFs {
			inUse[pv.VIF.ID] = true
		}
	}
	return inUse, nil
}

// addRecovered files a rediscovered port into its pool.
func (p *Pool) addRecovered(k key, v *kuryrv1.VIF, sgs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.vifs[v.ID] = v
	p.bucketFor(k).push(makeSGKey(sgs), v.ID)
}

// recoverBaremetal rediscovers pooled ports for a bare metal deployment.
//
// The pool a port belongs to is read from binding:host_id, which Neutron kept
// across the restart. A port with no binding information cannot be placed, so
// it is deleted rather than left to rot: the Python implementation did the
// same, on the grounds that a port nobody can account for is worse than a
// port that is gone.
func recoverBaremetal(ctx context.Context, p *Pool) error {
	logger := log.FromContext(ctx)

	inUse, err := p.inUsePortIDs(ctx)
	if err != nil {
		return err
	}

	filter := openstack.PortFilter{DeviceOwner: openstack.DeviceOwner, Tags: p.os.ResourceTags()}
	if p.cfg.Kubernetes.PortDebug {
		// With port_debug the pooled ports are named, so they can be found
		// precisely instead of by elimination.
		filter.Name = openstack.PoolPortName
	}

	ports, err := p.os.ListPorts(ctx, filter)
	if err != nil {
		return err
	}

	// Subports belong to a nested deployment sharing the same project; they
	// are not ours to pool here.
	subports, err := p.os.ListPorts(ctx,
		openstack.PortFilter{DeviceOwner: openstack.DeviceOwnerSubport, Tags: p.os.ResourceTags()})
	if err != nil {
		return err
	}
	isSubport := make(map[string]bool, len(subports))
	for _, sp := range subports {
		isSubport[sp.ID] = true
	}

	var recovered, deleted int
	for _, port := range ports {
		if inUse[port.ID] || isSubport[port.ID] {
			continue
		}
		if port.HostID == "" || port.VIFType == "" || port.VIFType == openstack.VIFTypeUnbound {
			// Nothing identifies which pool this belongs to, and an unbound
			// port would never reach ACTIVE anyway.
			logger.V(1).Info("deleting an unplaceable pooled port", "portID", port.ID)
			if err := p.os.DeletePort(ctx, port.ID); err != nil {
				logger.Error(err, "could not delete an unplaceable port", "portID", port.ID)
				continue
			}
			deleted++
			continue
		}

		v, err := p.vifFromPort(ctx, port)
		if err != nil {
			logger.V(1).Info("skipping a port that cannot be described", "portID", port.ID, "error", err)
			continue
		}

		k := key{Host: port.HostID, ProjectID: port.ProjectID, NetworkID: port.NetworkID}
		p.addRecovered(k, v, port.SecurityGroups)
		recovered++
	}

	logger.Info("baremetal pool recovery finished", "recovered", recovered, "deleted", deleted)
	return nil
}

// recoverNested rediscovers pooled subports by walking the trunks.
//
// This is the case pooling was really designed for: a subport stays attached
// to its trunk across a controller restart, so both the VLAN id and the node
// it belongs to survive, and every port can be put back exactly where it was.
func recoverNested(ctx context.Context, p *Pool) error {
	logger := log.FromContext(ctx)

	inUse, err := p.inUsePortIDs(ctx)
	if err != nil {
		return err
	}

	trunks, err := p.os.ListTrunks(ctx)
	if err != nil {
		return err
	}

	// Index the ports we might pool, so each trunk subport can be resolved
	// without a call of its own.
	candidates := map[string]*openstack.Port{}
	for _, owner := range []string{openstack.DeviceOwnerSubport, openstack.DeviceOwner} {
		filter := openstack.PortFilter{DeviceOwner: owner, Tags: p.os.ResourceTags()}
		if p.cfg.Kubernetes.PortDebug {
			filter.Name = openstack.PoolPortName
		}
		ports, err := p.os.ListPorts(ctx, filter)
		if err != nil {
			return err
		}
		for _, port := range ports {
			candidates[port.ID] = port
		}
	}

	var recovered int
	for _, trunk := range trunks {
		parent, err := p.os.GetPort(ctx, trunk.PortID)
		if err != nil {
			logger.V(1).Info("skipping a trunk whose parent port cannot be read",
				"trunkID", trunk.ID, "error", err)
			continue
		}
		hostIP := firstFixedIP(parent)
		if hostIP == "" {
			logger.V(1).Info("skipping a trunk whose parent has no address", "trunkID", trunk.ID)
			continue
		}

		for _, sub := range trunk.SubPorts {
			if inUse[sub.PortID] {
				continue
			}
			port, ok := candidates[sub.PortID]
			if !ok {
				// Attached to the trunk but not one of ours, or not free.
				continue
			}

			v, err := p.vifFromSubport(ctx, port, sub.SegmentationID, trunk.PortID)
			if err != nil {
				logger.V(1).Info("skipping a subport that cannot be described",
					"portID", port.ID, "error", err)
				continue
			}

			k := key{Host: hostIP, ProjectID: port.ProjectID, NetworkID: port.NetworkID}
			p.addRecovered(k, v, port.SecurityGroups)
			recovered++
		}
	}

	logger.Info("nested pool recovery finished", "recovered", recovered, "trunks", len(trunks))
	return nil
}

// vifFromPort rebuilds a baremetal VIF from a Neutron port.
func (p *Pool) vifFromPort(ctx context.Context, port *openstack.Port) (*kuryrv1.VIF, error) {
	subnets, network, err := p.describe(ctx, port)
	if err != nil {
		return nil, err
	}
	return vif.FromPort(port, subnets, network, vif.Options{
		DefaultOVSBridge:    p.cfg.NeutronDefaults.OVSBridge,
		VHostUserMountPoint: p.cfg.VHostUser.MountPoint,
	})
}

// vifFromSubport rebuilds a nested VIF, including the VLAN id the trunk
// still remembers.
func (p *Pool) vifFromSubport(ctx context.Context, port *openstack.Port,
	vlanID int, parentPortID string) (*kuryrv1.VIF, error) {

	subnets, network, err := p.describe(ctx, port)
	if err != nil {
		return nil, err
	}
	return vif.FromPortNestedVLAN(port, subnets, network, vlanID, parentPortID)
}

// describe resolves the subnets and network behind a port. Both are cached in
// the OpenStack client, so recovering hundreds of ports on one network costs
// one lookup.
func (p *Pool) describe(ctx context.Context, port *openstack.Port) (vif.SubnetSet, *openstack.Network, error) {
	if len(port.FixedIPs) == 0 {
		return nil, nil, fmt.Errorf("port %s has no fixed IPs", port.ID)
	}

	subnets := vif.SubnetSet{}
	for _, fip := range port.FixedIPs {
		if fip.SubnetID == "" {
			continue
		}
		if _, seen := subnets[fip.SubnetID]; seen {
			continue
		}
		sub, err := p.os.GetSubnet(ctx, fip.SubnetID)
		if err != nil {
			return nil, nil, err
		}
		subnets[fip.SubnetID] = sub
	}
	if len(subnets) == 0 {
		return nil, nil, fmt.Errorf("port %s has no resolvable subnets", port.ID)
	}

	network, err := p.os.GetNetwork(ctx, port.NetworkID)
	if err != nil {
		return nil, nil, err
	}
	return subnets, network, nil
}

func firstFixedIP(port *openstack.Port) string {
	for _, fip := range port.FixedIPs {
		if fip.IPAddress != "" {
			return fip.IPAddress
		}
	}
	return ""
}
