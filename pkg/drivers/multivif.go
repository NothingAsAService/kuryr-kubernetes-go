package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

// Annotations and keys from the Network Plumbing Working Group specification,
// which is what Multus and everything compatible with it uses.
const (
	// NetworksAnnotation lists the extra networks a pod asks for.
	NetworksAnnotation = "k8s.v1.cni.cncf.io/networks"
	// KuryrConfigAnnotation is where a NetworkAttachmentDefinition carries
	// the Kuryr-specific part of its configuration.
	KuryrConfigAnnotation = "openstack.org/kuryr-config"
)

// nadGVK identifies a NetworkAttachmentDefinition.
//
// It is read as an unstructured object on purpose: the CRD belongs to Multus,
// and depending on its Go types would tie this build to a Multus release for
// the sake of two annotation lookups.
var nadGVK = schema.GroupVersionKind{
	Group:   "k8s.cni.cncf.io",
	Version: "v1",
	Kind:    "NetworkAttachmentDefinition",
}

// MultiVIFDriver decides which extra interfaces a pod gets beyond its
// default one.
type MultiVIFDriver interface {
	// Name is the registered driver name.
	Name() string
	// AdditionalVIFs returns the extra ports for a pod, in the order the pod
	// asked for them. An empty result is the normal case.
	AdditionalVIFs(ctx context.Context, req PodVIFRequest) ([]kuryrv1.VIF, error)
}

// VIFDriverFor resolves a pod VIF driver by the alias a
// NetworkAttachmentDefinition names. An empty alias means the default driver,
// which is the pooled one when pooling is on.
type VIFDriverFor func(alias string) (PodVIFDriver, error)

// networkRef is one entry of the networks annotation.
type networkRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Interface string `json:"interface,omitempty"`
}

// nadConfig is the Kuryr part of a NetworkAttachmentDefinition.
type nadConfig struct {
	// SubnetID is the Neutron subnet to allocate the extra address from.
	SubnetID string `json:"subnetId,omitempty"`
	// DriverType names the pod VIF driver to use, by alias.
	DriverType string `json:"driverType,omitempty"`
}

// --- noop --------------------------------------------------------------

// noopMultiVIF gives every pod exactly one interface.
type noopMultiVIF struct{}

// Name is the registered driver name, used in logs and metrics.
func (noopMultiVIF) Name() string { return "noop" }

// AdditionalVIFs returns nothing: one interface per pod is the whole point of
// this driver.
func (noopMultiVIF) AdditionalVIFs(context.Context, PodVIFRequest) ([]kuryrv1.VIF, error) {
	return nil, nil
}

// --- npwg --------------------------------------------------------------

// npwgMultiVIF gives a pod the extra interfaces its annotation asks for.
type npwgMultiVIF struct {
	k8s     client.Client
	os      subnetReader
	cfg     subnetDefaults
	resolve VIFDriverFor
}

// subnetReader is the piece of the OpenStack client this driver needs. It is
// an interface so the annotation parsing and subnet resolution can be tested
// without a cloud.
type subnetReader interface {
	GetSubnet(ctx context.Context, id string) (*openstack.Subnet, error)
}

// subnetDefaults are the fallbacks when a NetworkAttachmentDefinition names
// no subnet.
type subnetDefaults struct {
	// PodSubnet is the cluster-wide default.
	PodSubnet string
	// PerDriver maps a driver alias to the subnet its ports come from.
	PerDriver map[string]string
}

// Name is the registered driver name, used in logs and metrics.
func (n *npwgMultiVIF) Name() string { return "npwg" }

// AdditionalVIFs creates one port per network the pod asks for.
//
// A failure partway through leaves the ports already created behind. That is
// deliberate: the caller records whatever came back and releases the lot if
// it cannot, which is the same contract the single-VIF path has.
func (n *npwgMultiVIF) AdditionalVIFs(ctx context.Context, req PodVIFRequest) ([]kuryrv1.VIF, error) {
	if req.Pod == nil {
		return nil, fmt.Errorf("the npwg multi-VIF driver needs a pod")
	}

	networks, err := parseNetworks(req.Pod.Annotations[NetworksAnnotation])
	if err != nil {
		return nil, fmt.Errorf("pod %s/%s: %w", req.Pod.Namespace, req.Pod.Name, err)
	}
	if len(networks) == 0 {
		return nil, nil
	}

	out := make([]kuryrv1.VIF, 0, len(networks))
	for _, network := range networks {
		namespace := network.Namespace
		if namespace == "" {
			namespace = req.Pod.Namespace
		}

		conf, err := n.readConfig(ctx, namespace, network.Name)
		if err != nil {
			return out, err
		}

		driver, err := n.resolve(conf.DriverType)
		if err != nil {
			return out, fmt.Errorf("network %s/%s: %w", namespace, network.Name, err)
		}

		subnets, err := n.subnetsFor(ctx, conf)
		if err != nil {
			return out, fmt.Errorf("network %s/%s: %w", namespace, network.Name, err)
		}

		v, err := driver.RequestVIF(ctx, PodVIFRequest{
			Pod:            req.Pod,
			ProjectID:      req.ProjectID,
			Subnets:        subnets,
			SecurityGroups: req.SecurityGroups,
			HostID:         req.HostID,
		})
		if err != nil {
			return out, fmt.Errorf("network %s/%s: requesting an extra port: %w",
				namespace, network.Name, err)
		}
		if v == nil {
			// A driver that declines is not an error, but it is unusual
			// enough to be worth not silently dropping the network.
			return out, fmt.Errorf("network %s/%s: the %s driver returned no port",
				namespace, network.Name, driver.Name())
		}
		out = append(out, *v)
	}

	return out, nil
}

// readConfig pulls the Kuryr configuration out of a
// NetworkAttachmentDefinition.
func (n *npwgMultiVIF) readConfig(ctx context.Context, namespace, name string) (nadConfig, error) {
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(nadGVK)

	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := n.k8s.Get(ctx, key, nad); err != nil {
		return nadConfig{}, fmt.Errorf("reading NetworkAttachmentDefinition %s: %w", key, err)
	}

	raw, ok := nad.GetAnnotations()[KuryrConfigAnnotation]
	if !ok {
		return nadConfig{}, fmt.Errorf(
			"NetworkAttachmentDefinition %s has no %s annotation, so Kuryr does not know "+
				"which subnet or driver to use for it", key, KuryrConfigAnnotation)
	}

	var conf nadConfig
	if err := json.Unmarshal([]byte(raw), &conf); err != nil {
		return nadConfig{}, fmt.Errorf("the %s annotation of NetworkAttachmentDefinition %s "+
			"is not valid JSON: %w", KuryrConfigAnnotation, key, err)
	}
	return conf, nil
}

// subnetsFor resolves which subnet an extra interface allocates from.
//
// The order is: whatever the NetworkAttachmentDefinition names, then the
// subnet mapped to the driver alias in kuryr.conf, then the default pod
// subnet.
func (n *npwgMultiVIF) subnetsFor(ctx context.Context, conf nadConfig) (vif.SubnetSet, error) {
	id := conf.SubnetID
	if id == "" && conf.DriverType != "" {
		id = n.cfg.PerDriver[conf.DriverType]
	}
	if id == "" {
		id = n.cfg.PodSubnet
	}
	if id == "" {
		return nil, fmt.Errorf("no subnet: the NetworkAttachmentDefinition names none, " +
			"[neutron_defaults]subnet_mapping has no entry for its driver, and " +
			"[neutron_defaults]pod_subnet is unset")
	}

	subnet, err := n.os.GetSubnet(ctx, id)
	if err != nil {
		return nil, err
	}
	return vif.SubnetSet{subnet.ID: subnet}, nil
}

// parseNetworks reads the networks annotation.
//
// The specification allows two forms: a JSON array of objects, and a
// comma-separated shorthand where each entry is "name" or "namespace/name".
// Both are accepted because both appear in real manifests.
func parseNetworks(annotation string) ([]networkRef, error) {
	annotation = strings.TrimSpace(annotation)
	if annotation == "" {
		return nil, nil
	}

	if strings.HasPrefix(annotation, "[") {
		var refs []networkRef
		if err := json.Unmarshal([]byte(annotation), &refs); err != nil {
			return nil, fmt.Errorf("the %s annotation is not a valid network list: %w",
				NetworksAnnotation, err)
		}
		for i := range refs {
			if refs[i].Name == "" {
				return nil, fmt.Errorf("entry %d of the %s annotation has no name", i,
					NetworksAnnotation)
			}
		}
		return refs, nil
	}

	var refs []networkRef
	for _, entry := range strings.Split(annotation, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, "/")
		switch len(parts) {
		case 1:
			refs = append(refs, networkRef{Name: parts[0]})
		case 2:
			refs = append(refs, networkRef{Namespace: parts[0], Name: parts[1]})
		default:
			return nil, fmt.Errorf("%q in the %s annotation is not a network name or a "+
				"namespace/name pair", entry, NetworksAnnotation)
		}
	}
	return refs, nil
}

// --- registry ----------------------------------------------------------

type multiVIFFactory func(Deps, VIFDriverFor) (MultiVIFDriver, error)

var multiVIFDrivers = map[string]multiVIFFactory{
	"noop": func(Deps, VIFDriverFor) (MultiVIFDriver, error) { return noopMultiVIF{}, nil },
	"npwg": newNPWGMultiVIF,
}

func newNPWGMultiVIF(d Deps, resolve VIFDriverFor) (MultiVIFDriver, error) {
	if d.K8s == nil {
		return nil, fmt.Errorf("the npwg multi-VIF driver needs a Kubernetes client")
	}
	if resolve == nil {
		return nil, fmt.Errorf("the npwg multi-VIF driver needs a way to build pod VIF drivers")
	}
	return &npwgMultiVIF{
		k8s:     d.K8s,
		os:      d.OpenStack,
		resolve: resolve,
		cfg: subnetDefaults{
			PodSubnet: d.Config.NeutronDefaults.PodSubnet,
			PerDriver: d.Config.NeutronDefaults.SubnetMapping,
		},
	}, nil
}

// NewMultiVIFDrivers builds the chain of multi-VIF drivers named in
// kuryr.conf.
//
// More than one may be configured, and their results are concatenated. That
// is how the Python implementation worked, and it lets a deployment combine,
// say, NPWG networks with a site-specific driver.
func NewMultiVIFDrivers(names []string, deps Deps, resolve VIFDriverFor) ([]MultiVIFDriver, error) {
	out := make([]MultiVIFDriver, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		f, ok := multiVIFDrivers[name]
		if !ok {
			return nil, unknownDriver("multi-VIF", name, slices.Sorted(maps.Keys(multiVIFDrivers)))
		}
		drv, err := f(deps, resolve)
		if err != nil {
			return nil, err
		}
		out = append(out, drv)
	}
	return out, nil
}
