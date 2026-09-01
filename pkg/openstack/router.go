package openstack

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
)

// AttachSubnetToRouter gives a namespace subnet a way off its own network.
//
// Attaching a subnet that is already attached returns a conflict from
// Neutron. That is the normal outcome when a reconcile runs twice, so it is
// reported as success rather than as an error.
func (c *Client) AttachSubnetToRouter(ctx context.Context, routerID, subnetID string) error {
	if routerID == "" {
		return fmt.Errorf("no router configured; set [namespace_subnet]pod_router")
	}
	if subnetID == "" {
		return fmt.Errorf("cannot attach an empty subnet id to router %s", routerID)
	}

	_, err := routers.AddInterface(ctx, c.Network, routerID,
		routers.AddInterfaceOpts{SubnetID: subnetID}).Extract()
	if err == nil {
		return nil
	}
	if IsConflict(err) {
		// Already attached. Neutron uses 409 for this, and re-running a
		// reconcile must not fail because the work was already done.
		return nil
	}
	return fmt.Errorf("attaching subnet %s to router %s: %w", subnetID, routerID, err)
}

// DetachSubnetFromRouter removes a subnet from a router.
//
// A subnet that is already detached, or a router or subnet that no longer
// exists, are all treated as success: teardown must be safe to retry.
func (c *Client) DetachSubnetFromRouter(ctx context.Context, routerID, subnetID string) error {
	if routerID == "" || subnetID == "" {
		return nil
	}

	_, err := routers.RemoveInterface(ctx, c.Network, routerID,
		routers.RemoveInterfaceOpts{SubnetID: subnetID}).Extract()
	if err == nil {
		return nil
	}
	if IsNotFound(err) || IsConflict(err) {
		return nil
	}
	return fmt.Errorf("detaching subnet %s from router %s: %w", subnetID, routerID, err)
}

// GetRouter checks that a router exists, so a misconfigured
// [namespace_subnet]pod_router is reported at startup rather than on the
// first pod.
func (c *Client) GetRouter(ctx context.Context, id string) error {
	if _, err := routers.Get(ctx, c.Network, id).Extract(); err != nil {
		return fmt.Errorf("reading router %s: %w", id, err)
	}
	return nil
}
