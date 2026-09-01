package pool

import (
	"context"

	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

// cloud is the slice of OpenStack the pool actually touches.
//
// Narrowing it to an interface is what makes the pool testable: the recycler,
// the repopulator and the recovery logic are the subtlest code in the
// controller, and none of it should need a live cloud to exercise.
type cloud interface {
	ListPorts(ctx context.Context, f openstack.PortFilter) ([]*openstack.Port, error)
	GetPort(ctx context.Context, id string) (*openstack.Port, error)
	UpdatePort(ctx context.Context, id string, u openstack.PortUpdate) (*openstack.Port, error)
	DeletePort(ctx context.Context, id string) error
	GetSubnet(ctx context.Context, id string) (*openstack.Subnet, error)
	GetNetwork(ctx context.Context, id string) (*openstack.Network, error)
	ListTrunks(ctx context.Context) ([]*openstack.Trunk, error)
	ResourceTags() []string
}

// The production client satisfies it.
var _ cloud = (*openstack.Client)(nil)
