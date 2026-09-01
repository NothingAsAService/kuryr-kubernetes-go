package openstack

import (
	"context"
	"errors"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/attachinterfaces"
)

// ErrNoCompute means Nova is not in the service catalog.
//
// Only the nested-dpdk driver needs it, so this is reported where that driver
// is built rather than at startup: a cluster that does not run DPDK workloads
// has no reason to care whether it can reach Nova.
var ErrNoCompute = errors.New("openstack: no Nova endpoint in the service catalog")

// AttachServerInterface asks Nova to give a server a new interface on a
// network, and returns the Neutron port behind it.
//
// This goes through the compute API rather than creating the port directly
// because the port has to be plugged into the node VM itself, which is a
// compute operation. Neutron would happily create the port, but nothing would
// attach it to the instance.
func (c *Client) AttachServerInterface(ctx context.Context, serverID, networkID string) (*Port, error) {
	if c.Compute == nil {
		return nil, ErrNoCompute
	}

	iface, err := attachinterfaces.Create(ctx, c.Compute, serverID, attachinterfaces.CreateOpts{
		NetworkID: networkID,
	}).Extract()
	if err != nil {
		return nil, fmt.Errorf("attaching an interface on network %s to server %s: %w",
			networkID, serverID, err)
	}

	// Nova reports only the port id and its addresses, so the full port is
	// read back: the VIF needs the MAC, the binding details and the fixed IPs
	// in the shape the rest of the code already handles.
	port, err := c.GetPort(ctx, iface.PortID)
	if err != nil {
		return nil, fmt.Errorf("reading port %s that Nova attached to server %s: %w",
			iface.PortID, serverID, err)
	}
	return port, nil
}

// DetachServerInterface removes an interface from a server, which deletes the
// port with it. Detaching an interface that is already gone is not an error:
// a retried release has to converge, not fail.
func (c *Client) DetachServerInterface(ctx context.Context, serverID, portID string) error {
	if c.Compute == nil {
		return ErrNoCompute
	}

	if err := attachinterfaces.Delete(ctx, c.Compute, serverID, portID).ExtractErr(); err != nil {
		if IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("detaching interface %s from server %s: %w", portID, serverID, err)
	}
	return nil
}
