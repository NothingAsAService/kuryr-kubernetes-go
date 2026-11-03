package drivers

import (
	"context"
	"fmt"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

func init() {
	RegisterPodVIFDriver("neutron-vif", newNeutronVIFDriver)
	RegisterPodVIFDriver("nested-vlan", newNestedVLANDriver)
	RegisterPodVIFDriver("nested-macvlan", newNestedMacvlanDriver)
	RegisterPodVIFDriver("nested-dpdk", newNestedDPDKDriver)
}

// portName builds the Neutron port name for a pod. Naming ports after pods is
// optional because it costs an extra write on every create, but it is close to
// mandatory for anyone who has to debug a cluster from the OpenStack side.
func portName(pod *corev1.Pod, portDebug bool) string {
	if !portDebug || pod == nil {
		return ""
	}
	return pod.Namespace + "/" + pod.Name
}

func deviceID(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	return string(pod.UID)
}

// --- neutron-vif: bare metal -------------------------------------------

// neutronVIFDriver creates ports bound directly to the Kubernetes node, which
// is also the compute host running the Neutron OVS agent.
type neutronVIFDriver struct {
	os   *openstack.Client
	cfg  *config.Config
	opts vif.Options
}

func newNeutronVIFDriver(d Deps) (PodVIFDriver, error) {
	return &neutronVIFDriver{
		os:  d.OpenStack,
		cfg: d.Config,
		opts: vif.Options{
			DefaultOVSBridge:    d.Config.NeutronDefaults.OVSBridge,
			VHostUserMountPoint: d.Config.VHostUser.MountPoint,
		},
	}, nil
}

// Name is the registered driver name, used in logs and metrics.
func (n *neutronVIFDriver) Name() string { return "neutron-vif" }

func (n *neutronVIFDriver) portSpec(req PodVIFRequest, unbound bool) (openstack.PortSpec, error) {
	networkID, ok := req.Subnets.NetworkID()
	if !ok {
		return openstack.PortSpec{}, fmt.Errorf("subnets for pod span more than one network")
	}

	spec := openstack.PortSpec{
		NetworkID:      networkID,
		ProjectID:      req.ProjectID,
		DeviceOwner:    openstack.DeviceOwner,
		FixedIPs:       vif.FixedIPsFor(req.Subnets),
		SecurityGroups: req.SecurityGroups,
	}

	if unbound {
		// A pooled port belongs to no pod yet: no device_id, and a generic
		// name so the restart-recovery path can recognise it.
		if n.cfg.Kubernetes.PortDebug {
			spec.Name = openstack.PoolPortName
		}
		// It is still bound to the host, because that is what makes Neutron
		// program the flows ahead of time and reach ACTIVE.
		spec.HostID = req.HostID
		return spec, nil
	}

	spec.Name = portName(req.Pod, n.cfg.Kubernetes.PortDebug)
	spec.DeviceID = deviceID(req.Pod)
	spec.HostID = req.HostID
	if spec.HostID == "" && req.Pod != nil {
		spec.HostID = req.Pod.Spec.NodeName
	}
	return spec, nil
}

// RequestVIF creates one port bound to the node the pod was scheduled on.
func (n *neutronVIFDriver) RequestVIF(ctx context.Context, req PodVIFRequest) (*kuryrv1.VIF, error) {
	spec, err := n.portSpec(req, false)
	if err != nil {
		return nil, err
	}
	port, err := n.os.CreatePort(ctx, spec)
	if err != nil {
		return nil, err
	}

	network, err := n.os.GetNetwork(ctx, port.NetworkID)
	if err != nil {
		// The port exists but we cannot describe it; do not leak it.
		_ = n.os.DeletePort(ctx, port.ID)
		return nil, err
	}
	return vif.FromPort(port, req.Subnets, network, n.opts)
}

// RequestVIFs creates count unbound ports in one bulk call, which is what
// makes a pool cheaper than creating ports one pod at a time.
func (n *neutronVIFDriver) RequestVIFs(ctx context.Context, req PodVIFRequest,
	count int) ([]*kuryrv1.VIF, error) {

	spec, err := n.portSpec(req, true)
	if err != nil {
		return nil, err
	}
	ports, err := n.os.CreatePortsBulk(ctx, spec, count)
	if err != nil {
		return nil, err
	}
	if len(ports) == 0 {
		return nil, nil
	}

	network, err := n.os.GetNetwork(ctx, ports[0].NetworkID)
	if err != nil {
		ids := make([]string, 0, len(ports))
		for _, p := range ports {
			ids = append(ids, p.ID)
		}
		_ = n.os.DeletePorts(ctx, ids)
		return nil, err
	}

	out := make([]*kuryrv1.VIF, 0, len(ports))
	for _, p := range ports {
		v, err := vif.FromPort(p, req.Subnets, network, n.opts)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// ReleaseVIF deletes the port. Nothing else holds a reference to it.
func (n *neutronVIFDriver) ReleaseVIF(ctx context.Context, v *kuryrv1.VIF, _ PodVIFRequest) error {
	return n.os.DeletePort(ctx, v.ID)
}

// ActivateVIF re-reads the port and reports ErrPortNotReady until Neutron has
// brought it to ACTIVE.
func (n *neutronVIFDriver) ActivateVIF(ctx context.Context, v *kuryrv1.VIF, _ PodVIFRequest) error {
	if v.Active {
		return nil
	}
	port, err := n.os.GetPort(ctx, v.ID)
	if err != nil {
		return err
	}
	if !port.IsActive() {
		return &ErrPortNotReady{PortID: v.ID, Status: port.Status}
	}
	v.Active = true
	return nil
}

// UpdateVIFSecurityGroups re-applies groups to a port that is already live,
// which is what happens when a NetworkPolicy starts or stops matching the pod.
func (n *neutronVIFDriver) UpdateVIFSecurityGroups(ctx context.Context, v *kuryrv1.VIF,
	sgs []string) error {

	_, err := n.os.UpdatePort(ctx, v.ID, openstack.PortUpdate{SecurityGroups: &sgs})
	return err
}

// --- nested-vlan: pods inside node VMs ---------------------------------

// nestedVLANDriver attaches pod ports to the node VM trunk as VLAN subports.
type nestedVLANDriver struct {
	os  *openstack.Client
	cfg *config.Config

	vlans *vlanAllocator
	// trunkCache maps a node host IP to its trunk, so that creating N pods on
	// a node does not perform N trunk lookups.
	trunkCache sync.Map // string -> *openstack.Trunk
}

func newNestedVLANDriver(d Deps) (PodVIFDriver, error) {
	if !d.OpenStack.SupportsTrunk() {
		return nil, fmt.Errorf(
			"pod_vif_driver=nested-vlan requires the Neutron trunk extension, which this cloud does not expose")
	}
	return &nestedVLANDriver{os: d.OpenStack, cfg: d.Config, vlans: newVLANAllocator()}, nil
}

// Name is the registered driver name, used in logs and metrics.
func (n *nestedVLANDriver) Name() string { return "nested-vlan" }

// hostIPFor resolves the address of the VM a pod runs in.
func hostIPFor(req PodVIFRequest) (string, error) {
	if req.HostID != "" {
		return req.HostID, nil
	}
	if req.Pod == nil || req.Pod.Status.HostIP == "" {
		return "", fmt.Errorf("pod has no hostIP yet; it is not scheduled or kubelet has not reported")
	}
	return req.Pod.Status.HostIP, nil
}

// trunkFor finds, and caches, the trunk belonging to the VM at hostIP.
func (n *nestedVLANDriver) trunkFor(ctx context.Context, hostIP string) (*openstack.Trunk, error) {
	if cached, ok := n.trunkCache.Load(hostIP); ok {
		return cached.(*openstack.Trunk), nil
	}

	parent, err := n.os.FindPortByFixedIP(ctx, hostIP, n.cfg.PodVIFNested.WorkerNodesSubnets)
	if err != nil {
		return nil, fmt.Errorf("finding the node VM port for %s: %w", hostIP, err)
	}
	trunk, err := n.os.FindTrunkByParentPort(ctx, parent.ID)
	if err != nil {
		return nil, err
	}

	// Seed the VLAN allocator from what is already attached, so a controller
	// restart does not hand out ids that are in use.
	n.vlans.seed(trunk.ID, trunk.UsedVLANs())

	n.trunkCache.Store(hostIP, trunk)
	return trunk, nil
}

func (n *nestedVLANDriver) portSpec(req PodVIFRequest, unbound bool) (openstack.PortSpec, error) {
	networkID, ok := req.Subnets.NetworkID()
	if !ok {
		return openstack.PortSpec{}, fmt.Errorf("subnets for pod span more than one network")
	}
	spec := openstack.PortSpec{
		NetworkID: networkID,
		ProjectID: req.ProjectID,
		// A subport is never bound to a host: the trunk parent is, and the
		// subport rides on it. Setting binding:host_id here would make
		// Neutron try to bind it separately and fail.
		DeviceOwner:    openstack.DeviceOwner,
		FixedIPs:       vif.FixedIPsFor(req.Subnets),
		SecurityGroups: req.SecurityGroups,
	}
	if unbound {
		if n.cfg.Kubernetes.PortDebug {
			spec.Name = openstack.PoolPortName
		}
		return spec, nil
	}
	spec.Name = portName(req.Pod, n.cfg.Kubernetes.PortDebug)
	spec.DeviceID = deviceID(req.Pod)
	return spec, nil
}

// RequestVIF creates one port and attaches it to the node VM's trunk as a VLAN
// subport.
func (n *nestedVLANDriver) RequestVIF(ctx context.Context, req PodVIFRequest) (*kuryrv1.VIF, error) {
	vifs, err := n.requestN(ctx, req, 1, false)
	if err != nil {
		return nil, err
	}
	if len(vifs) == 0 {
		return nil, fmt.Errorf("no VLAN ids available on the node trunk")
	}
	return vifs[0], nil
}

// RequestVIFs creates count unbound subports in one pass. See requestN for why
// the VLAN ids are reserved before the ports are created.
func (n *nestedVLANDriver) RequestVIFs(ctx context.Context, req PodVIFRequest,
	count int) ([]*kuryrv1.VIF, error) {

	return n.requestN(ctx, req, count, true)
}

// requestN creates count ports and attaches them to the trunk in one call.
//
// The ordering matters and is the reverse of what feels natural: VLAN ids are
// reserved first, ports created second, subports attached third. Reserving
// first means a failure anywhere later can hand every id back, whereas
// creating ports first would leave them stranded if the trunk rejected them.
func (n *nestedVLANDriver) requestN(ctx context.Context, req PodVIFRequest,
	count int, unbound bool) ([]*kuryrv1.VIF, error) {

	hostIP, err := hostIPFor(req)
	if err != nil {
		return nil, err
	}
	trunk, err := n.trunkFor(ctx, hostIP)
	if err != nil {
		return nil, err
	}

	ids := n.vlans.allocate(trunk.ID, count)
	if len(ids) == 0 {
		return nil, fmt.Errorf("trunk %s has no free VLAN ids (limit is 4094 pods per node)", trunk.ID)
	}
	release := func() { n.vlans.release(trunk.ID, ids...) }

	spec, err := n.portSpec(req, unbound)
	if err != nil {
		release()
		return nil, err
	}

	ports, err := n.os.CreatePortsBulk(ctx, spec, len(ids))
	if err != nil {
		release()
		return nil, err
	}
	// Neutron may return fewer ports than asked; hand back the surplus ids.
	if len(ports) < len(ids) {
		n.vlans.release(trunk.ID, ids[len(ports):]...)
		ids = ids[:len(ports)]
	}

	subports := make([]openstack.SubPort, 0, len(ports))
	for i, p := range ports {
		subports = append(subports, openstack.SubPort{
			PortID:           p.ID,
			SegmentationID:   ids[i],
			SegmentationType: openstack.SegmentationTypeVLAN,
		})
	}

	if err := n.os.AddSubPorts(ctx, trunk.ID, subports); err != nil {
		portIDs := make([]string, 0, len(ports))
		for _, p := range ports {
			portIDs = append(portIDs, p.ID)
		}
		_ = n.os.DeletePorts(ctx, portIDs)
		release()
		// The cached trunk may be stale if the ids collided with something we
		// did not know about; drop it so the next attempt re-reads.
		n.trunkCache.Delete(hostIP)
		return nil, fmt.Errorf("attaching subports to trunk %s: %w", trunk.ID, err)
	}

	network, err := n.os.GetNetwork(ctx, ports[0].NetworkID)
	if err != nil {
		return nil, err
	}

	out := make([]*kuryrv1.VIF, 0, len(ports))
	for i, p := range ports {
		v, err := vif.FromPortNestedVLAN(p, req.Subnets, network, ids[i], trunk.PortID)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// ReleaseVIF detaches the subport from the trunk before deleting the port.
// Deleting a still-attached subport leaves the trunk holding a dead VLAN id.
func (n *nestedVLANDriver) ReleaseVIF(ctx context.Context, v *kuryrv1.VIF, req PodVIFRequest) error {
	// Detach before deleting. A port deleted while still attached leaves the
	// trunk holding a dangling subport entry and the VLAN id unusable.
	if trunk, err := n.trunkForRelease(ctx, v, req); err == nil && trunk != nil {
		if err := n.os.RemoveSubPorts(ctx, trunk.ID, []string{v.ID}); err != nil {
			return err
		}
		n.vlans.release(trunk.ID, int(v.VLANID))
	}
	return n.os.DeletePort(ctx, v.ID)
}

// trunkForRelease finds the trunk to detach from, preferring the parent port
// recorded on the VIF. The Python implementation had to scan every trunk in
// the project here; recording ParentPortID at creation makes it a lookup.
func (n *nestedVLANDriver) trunkForRelease(ctx context.Context, v *kuryrv1.VIF,
	req PodVIFRequest) (*openstack.Trunk, error) {

	if v.ParentPortID != "" {
		return n.os.FindTrunkByParentPort(ctx, v.ParentPortID)
	}
	hostIP, err := hostIPFor(req)
	if err != nil {
		return nil, err
	}
	return n.trunkFor(ctx, hostIP)
}

// ActivateVIF re-reads the port and reports ErrPortNotReady until Neutron has
// brought it to ACTIVE.
func (n *nestedVLANDriver) ActivateVIF(ctx context.Context, v *kuryrv1.VIF, _ PodVIFRequest) error {
	if v.Active {
		return nil
	}
	port, err := n.os.GetPort(ctx, v.ID)
	if err != nil {
		return err
	}
	if !port.IsActive() {
		return &ErrPortNotReady{PortID: v.ID, Status: port.Status}
	}
	v.Active = true
	return nil
}

// UpdateVIFSecurityGroups re-applies groups to a subport that is already live.
func (n *nestedVLANDriver) UpdateVIFSecurityGroups(ctx context.Context, v *kuryrv1.VIF,
	sgs []string) error {

	_, err := n.os.UpdatePort(ctx, v.ID, openstack.PortUpdate{SecurityGroups: &sgs})
	return err
}

// --- nested-macvlan ----------------------------------------------------

// nestedMacvlanDriver attaches pods as macvlan children of the node VM
// uplink. There is no trunk and no VLAN id, so no per-node port limit, but
// Neutron cannot filter the traffic either.
type nestedMacvlanDriver struct {
	os  *openstack.Client
	cfg *config.Config
}

func newNestedMacvlanDriver(d Deps) (PodVIFDriver, error) {
	return &nestedMacvlanDriver{os: d.OpenStack, cfg: d.Config}, nil
}

// Name is the registered driver name, used in logs and metrics.
func (n *nestedMacvlanDriver) Name() string { return "nested-macvlan" }

// parentPortFor resolves the node VM's own Neutron port, which is the one
// whose allowed address pairs have to carry every pod address on the node.
func (n *nestedMacvlanDriver) parentPortFor(ctx context.Context,
	req PodVIFRequest) (*openstack.Port, error) {

	hostIP, err := hostIPFor(req)
	if err != nil {
		return nil, err
	}
	parent, err := n.os.FindPortByFixedIP(ctx, hostIP, n.cfg.PodVIFNested.WorkerNodesSubnets)
	if err != nil {
		return nil, fmt.Errorf("finding the node VM port for %s: %w", hostIP, err)
	}
	return parent, nil
}

// addressPairsOf is the pairs a pod port needs on the node VM uplink: one per
// address it holds, all carrying the pod port's MAC, which is the address the
// macvlan child inside the pod will send from.
func addressPairsOf(port *openstack.Port) []openstack.AddressPair {
	out := make([]openstack.AddressPair, 0, len(port.FixedIPs))
	for _, fip := range port.FixedIPs {
		if fip.IPAddress == "" {
			continue
		}
		out = append(out, openstack.AddressPair{
			IPAddress:  fip.IPAddress,
			MACAddress: port.MACAddress,
		})
	}
	return out
}

// RequestVIF creates one port and records it as an allowed address pair on the
// node VM uplink, which is what lets a macvlan child use it.
//
// Without the pair Neutron's anti-spoofing rules drop every packet the pod
// sends: the traffic leaves the node VM's port carrying an address that port
// does not own. The pair is what makes this driver work at all, so a failure
// to add it deletes the port rather than handing back one nothing can use.
func (n *nestedMacvlanDriver) RequestVIF(ctx context.Context, req PodVIFRequest) (*kuryrv1.VIF, error) {
	networkID, ok := req.Subnets.NetworkID()
	if !ok {
		return nil, fmt.Errorf("subnets for pod span more than one network")
	}

	// Resolve the parent before creating anything: a pod that is not
	// scheduled yet has nowhere for its address to be allowed, and finding
	// that out after the port exists would mean cleaning it up again.
	parent, err := n.parentPortFor(ctx, req)
	if err != nil {
		return nil, err
	}

	port, err := n.os.CreatePort(ctx, openstack.PortSpec{
		NetworkID:      networkID,
		ProjectID:      req.ProjectID,
		Name:           portName(req.Pod, n.cfg.Kubernetes.PortDebug),
		DeviceID:       deviceID(req.Pod),
		DeviceOwner:    openstack.DeviceOwner,
		FixedIPs:       vif.FixedIPsFor(req.Subnets),
		SecurityGroups: req.SecurityGroups,
	})
	if err != nil {
		return nil, err
	}

	if err := n.allowAddresses(ctx, parent.ID, port); err != nil {
		_ = n.os.DeletePort(ctx, port.ID)
		return nil, err
	}

	network, err := n.os.GetNetwork(ctx, port.NetworkID)
	if err != nil {
		_ = n.denyAddresses(ctx, parent.ID, port)
		_ = n.os.DeletePort(ctx, port.ID)
		return nil, err
	}
	return vif.FromPortNestedMacvlan(port, req.Subnets, network)
}

// allowAddresses adds a pod port's addresses to the node VM uplink.
func (n *nestedMacvlanDriver) allowAddresses(ctx context.Context, parentID string,
	port *openstack.Port) error {

	wanted := addressPairsOf(port)
	if len(wanted) == 0 {
		return fmt.Errorf("port %s has no address to allow on the node VM uplink", port.ID)
	}

	err := n.os.UpdateAllowedAddressPairs(ctx, parentID,
		func(pairs []openstack.AddressPair) []openstack.AddressPair {
			for _, want := range wanted {
				if !slices.Contains(pairs, want) {
					pairs = append(pairs, want)
				}
			}
			return pairs
		})
	if err != nil {
		return fmt.Errorf("allowing the addresses of port %s on the node VM port %s: %w",
			port.ID, parentID, err)
	}
	return nil
}

// denyAddresses takes a pod port's addresses back off the node VM uplink.
//
// Leaving them would let a later pod that happened to be given the same
// address send from a node it is not on, so this is not merely tidying up.
func (n *nestedMacvlanDriver) denyAddresses(ctx context.Context, parentID string,
	port *openstack.Port) error {

	doomed := addressPairsOf(port)
	if len(doomed) == 0 {
		return nil
	}

	err := n.os.UpdateAllowedAddressPairs(ctx, parentID,
		func(pairs []openstack.AddressPair) []openstack.AddressPair {
			return slices.DeleteFunc(pairs, func(p openstack.AddressPair) bool {
				return slices.Contains(doomed, p)
			})
		})
	if err != nil {
		return fmt.Errorf("removing the addresses of port %s from the node VM port %s: %w",
			port.ID, parentID, err)
	}
	return nil
}

// RequestVIFs is not supported, as in the Python implementation, whose
// NestedMacvlanPodVIFDriver.request_vifs raises NotImplementedError. Every
// port needs its own read-modify-write against the node VM uplink's allowed
// address pairs, so there is no such thing as an interchangeable unbound one
// to pool. The configuration is still accepted, as it is upstream; a pool
// asked to fill itself reports this error instead.
func (n *nestedMacvlanDriver) RequestVIFs(context.Context, PodVIFRequest, int) ([]*kuryrv1.VIF, error) {
	return nil, fmt.Errorf(
		"the nested-macvlan driver cannot create unbound ports in bulk; set [kubernetes]vif_pool_driver=noop")
}

// ReleaseVIF removes the address pair from the node VM uplink, then deletes
// the port.
func (n *nestedMacvlanDriver) ReleaseVIF(ctx context.Context, v *kuryrv1.VIF, req PodVIFRequest) error {
	// The port is read back rather than rebuilt from the VIF because the
	// pairs were written from its fixed IPs and MAC, and they have to be
	// removed by exactly the same values.
	port, err := n.os.GetPort(ctx, v.ID)
	switch {
	case err == nil:
		parent, perr := n.parentPortFor(ctx, req)
		if perr != nil {
			// With no pod there is no host IP to find the parent by. Deleting
			// the port still frees the address, and the stale pair is
			// harmless until something else claims that address.
			log.FromContext(ctx).V(1).Info(
				"could not find the node VM port to remove the allowed address pair",
				"portID", v.ID, "reason", perr)
			break
		}
		if derr := n.denyAddresses(ctx, parent.ID, port); derr != nil {
			return derr
		}
	case openstack.IsNotFound(err):
		// Already gone, and its pairs with it.
	default:
		return err
	}

	return n.os.DeletePort(ctx, v.ID)
}

// ActivateVIF marks the VIF active straight away, as Python does. The port is
// only an IPAM reservation: nothing ever binds it, so Neutron never brings it
// to ACTIVE, and waiting for that would hang every pod in CNI ADD.
func (n *nestedMacvlanDriver) ActivateVIF(_ context.Context, v *kuryrv1.VIF, _ PodVIFRequest) error {
	v.Active = true
	return nil
}

// UpdateVIFSecurityGroups re-applies groups to the port. Neutron does not
// filter macvlan traffic, so this records intent more than it enforces it.
func (n *nestedMacvlanDriver) UpdateVIFSecurityGroups(ctx context.Context, v *kuryrv1.VIF,
	sgs []string) error {

	_, err := n.os.UpdatePort(ctx, v.ID, openstack.PortUpdate{SecurityGroups: &sgs})
	return err
}

// --- nested-dpdk -------------------------------------------------------

// nestedDPDKDriver hands a pod an interface on the node VM that a userspace
// driver in the pod then takes over.
//
// It is the one pod VIF driver that goes through Nova rather than Neutron. The
// interface has to be plugged into the node VM itself, and only the compute
// API can do that; creating the port directly would leave it attached to
// nothing. That is also why there is no trunk and no VLAN id here.
type nestedDPDKDriver struct {
	os  *openstack.Client
	cfg *config.Config
}

func newNestedDPDKDriver(d Deps) (PodVIFDriver, error) {
	if d.OpenStack.Compute == nil {
		return nil, fmt.Errorf(
			"pod_vif_driver=nested-dpdk attaches interfaces through Nova, which is not in the service catalog")
	}
	return &nestedDPDKDriver{os: d.OpenStack, cfg: d.Config}, nil
}

// Name is the registered driver name, used in logs and metrics.
func (n *nestedDPDKDriver) Name() string { return "nested-dpdk" }

// serverFor resolves the Nova instance the pod runs in, by way of the Neutron
// port holding the node's address.
func (n *nestedDPDKDriver) serverFor(ctx context.Context, req PodVIFRequest) (string, error) {
	hostIP, err := hostIPFor(req)
	if err != nil {
		return "", err
	}
	parent, err := n.os.FindPortByFixedIP(ctx, hostIP, n.cfg.PodVIFNested.WorkerNodesSubnets)
	if err != nil {
		return "", fmt.Errorf("finding the node VM port for %s: %w", hostIP, err)
	}
	if parent.DeviceID == "" {
		return "", fmt.Errorf("the node VM port for %s has no device_id, so its instance is unknown", hostIP)
	}
	return parent.DeviceID, nil
}

// RequestVIF asks Nova to attach an interface on the pod's network to the node
// VM, and describes the resulting port as a DPDK VIF.
func (n *nestedDPDKDriver) RequestVIF(ctx context.Context, req PodVIFRequest) (*kuryrv1.VIF, error) {
	networkID, ok := req.Subnets.NetworkID()
	if !ok {
		return nil, fmt.Errorf("subnets for pod span more than one network")
	}
	serverID, err := n.serverFor(ctx, req)
	if err != nil {
		return nil, err
	}

	port, err := n.os.AttachServerInterface(ctx, serverID, networkID)
	if err != nil {
		return nil, err
	}

	network, err := n.os.GetNetwork(ctx, port.NetworkID)
	if err != nil {
		// The interface exists but cannot be described; detach it rather than
		// leave the node VM holding one nothing will ever use.
		if detachErr := n.os.DetachServerInterface(ctx, serverID, port.ID); detachErr != nil {
			return nil, fmt.Errorf("%w (and detaching the interface again failed: %w)", err, detachErr)
		}
		return nil, err
	}
	return vif.FromPortNestedDPDK(port, req.Subnets, network)
}

// RequestVIFs is not supported: an interface has to be attached to a specific
// node VM, so there is no such thing as an interchangeable unbound one to pool.
// Pooling is refused at startup rather than here, because a pool that cannot
// be filled is a configuration mistake, not a runtime condition.
func (n *nestedDPDKDriver) RequestVIFs(context.Context, PodVIFRequest, int) ([]*kuryrv1.VIF, error) {
	return nil, fmt.Errorf(
		"the nested-dpdk driver cannot create unbound ports in bulk; set [kubernetes]vif_pool_driver=noop")
}

// ReleaseVIF detaches the interface from the node VM, which deletes the port.
func (n *nestedDPDKDriver) ReleaseVIF(ctx context.Context, v *kuryrv1.VIF, req PodVIFRequest) error {
	serverID, err := n.serverFor(ctx, req)
	if err != nil {
		return err
	}
	return n.os.DetachServerInterface(ctx, serverID, v.ID)
}

// ActivateVIF marks the VIF active without asking Neutron.
//
// Nova reported success when it attached the interface, and the node-side
// driver then rebinds the device to a userspace driver -- at which point the
// kernel no longer owns it and Neutron has nothing left to report a status
// from. Waiting for ACTIVE here would wait forever.
func (n *nestedDPDKDriver) ActivateVIF(_ context.Context, v *kuryrv1.VIF, _ PodVIFRequest) error {
	v.Active = true
	return nil
}

// UpdateVIFSecurityGroups re-applies groups to the port behind the interface.
func (n *nestedDPDKDriver) UpdateVIFSecurityGroups(ctx context.Context, v *kuryrv1.VIF,
	sgs []string) error {

	_, err := n.os.UpdatePort(ctx, v.ID, openstack.PortUpdate{SecurityGroups: &sgs})
	return err
}
