package openstack

import (
	"context"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions"
	"github.com/gophercloud/gophercloud/v2/pagination"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Neutron extension aliases Kuryr cares about.
const (
	extTrunk           = "trunk"
	extTagPorts        = "tag-ports-during-bulk-creation"
	extStandardAttrTag = "standard-attr-tag"
)

// detectCapabilities probes the Neutron extension list once at startup.
//
// The Python implementation ran the equivalent check (check_tag_on_creation)
// lazily and logged a warning; doing it up front means the port drivers can
// branch on a plain boolean instead of re-probing.
func detectCapabilities(ctx context.Context, network *gophercloud.ServiceClient) capabilities {
	logger := log.FromContext(ctx).WithName("openstack")

	aliases := map[string]bool{}
	err := extensions.List(network).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		list, err := extensions.ExtractExtensions(page)
		if err != nil {
			return false, err
		}
		for _, e := range list {
			aliases[e.Alias] = true
		}
		return true, nil
	})
	if err != nil {
		// Not fatal: fall back to the conservative assumption that nothing
		// optional is available. Tagging then happens as a second call and
		// the nested-vlan driver will fail loudly if it is actually needed.
		logger.Error(err, "could not list Neutron extensions, assuming none of the optional ones are present")
		return capabilities{}
	}

	caps := capabilities{
		Trunk:       aliases[extTrunk],
		TagOnCreate: aliases[extTagPorts] && aliases[extStandardAttrTag],
	}

	if caps.TagOnCreate {
		logger.Info("Neutron supports tagging during bulk port creation")
	} else {
		logger.Info("Neutron does not support tagging during bulk port creation; " +
			"Kuryr will tag resources in a second call")
	}
	if !caps.Trunk {
		logger.Info("Neutron trunk extension is absent; the nested-vlan VIF driver will not work")
	}

	return caps
}
