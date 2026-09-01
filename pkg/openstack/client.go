// Package openstack wraps the OpenStack APIs Kuryr needs.
//
// This replaces kuryr_kubernetes/clients.py, which handed callers raw
// openstacksdk proxies. Here the surface is narrowed to the calls Kuryr
// actually makes, so the rest of the codebase never has to know gophercloud.
package openstack

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"

	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
)

// Client holds the per-service clients Kuryr talks to.
type Client struct {
	Provider *gophercloud.ProviderClient

	// Network is the Neutron v2 endpoint.
	Network *gophercloud.ServiceClient
	// LoadBalancer is the Octavia v2 endpoint. It is nil when Octavia is not
	// present in the catalog, which is legitimate for a cluster that does not
	// use the service handlers.
	LoadBalancer *gophercloud.ServiceClient
	// Compute is the Nova v2 endpoint, nil when it is not in the catalog. Only
	// the nested-dpdk driver needs it: attaching an interface to the node VM
	// is a compute operation, not a Neutron one.
	Compute *gophercloud.ServiceClient

	// Tags is applied to every resource Kuryr creates, so that a cluster
	// teardown can find its own leftovers without touching anything else in
	// a shared project.
	Tags []string

	caps capabilities
}

// New authenticates against Keystone and resolves the service catalog.
func New(ctx context.Context, cfg *config.Config) (*Client, error) {
	if cfg.Auth.AuthURL == "" {
		return nil, fmt.Errorf("openstack: no auth_url configured; set [neutron]auth_url or OS_AUTH_URL")
	}

	ao := gophercloud.AuthOptions{
		IdentityEndpoint:            cfg.Auth.AuthURL,
		Username:                    cfg.Auth.Username,
		UserID:                      cfg.Auth.UserID,
		Password:                    cfg.Auth.Password,
		TenantName:                  cfg.Auth.ProjectName,
		TenantID:                    cfg.Auth.ProjectID,
		DomainName:                  cfg.Auth.UserDomainName,
		ApplicationCredentialID:     cfg.Auth.ApplicationCredentialID,
		ApplicationCredentialSecret: cfg.Auth.ApplicationCredentialSecret,
		// Kuryr is long lived and its token will expire many times over the
		// life of a controller, so reauthentication is not optional.
		AllowReauth: true,
	}
	if cfg.Auth.ProjectDomainName != "" {
		ao.Scope = &gophercloud.AuthScope{
			ProjectName: cfg.Auth.ProjectName,
			ProjectID:   cfg.Auth.ProjectID,
			DomainName:  cfg.Auth.ProjectDomainName,
		}
	}

	provider, err := openstack.NewClient(cfg.Auth.AuthURL)
	if err != nil {
		return nil, fmt.Errorf("openstack: building provider: %w", err)
	}

	httpClient, err := buildHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	provider.HTTPClient = *httpClient

	if err := openstack.Authenticate(ctx, provider, ao); err != nil {
		return nil, fmt.Errorf("openstack: authenticating to %s: %w", cfg.Auth.AuthURL, err)
	}

	eo := gophercloud.EndpointOpts{Region: cfg.Auth.Region}

	network, err := openstack.NewNetworkV2(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("openstack: no Neutron endpoint in catalog: %w", err)
	}

	c := &Client{
		Provider: provider,
		Network:  network,
		Tags:     cfg.NeutronDefaults.ResourceTags,
	}

	// Octavia is optional. A cluster running only the pod handlers never
	// touches it, and failing to start over a missing endpoint would be
	// unhelpful.
	if lb, err := openstack.NewLoadBalancerV2(provider, eo); err == nil {
		c.LoadBalancer = lb
	}

	// Nova is optional for the same reason: only nested-dpdk asks for it, and
	// that driver reports the missing endpoint itself.
	if compute, err := openstack.NewComputeV2(provider, eo); err == nil {
		c.Compute = compute
	}

	c.caps = detectCapabilities(ctx, network)
	return c, nil
}

// buildHTTPClient honours the TLS settings from kuryr.conf. Connection pool
// sizes are raised well above Go defaults: a controller reconciling a few
// hundred pods concurrently will otherwise serialise on idle connections, the
// same problem the Python version solved by mounting a TCPKeepAliveAdapter
// with pool_maxsize=1000.
func buildHTTPClient(cfg *config.Config) (*http.Client, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.Auth.Insecure, //nolint:gosec // operator opt-in
		MinVersion:         tls.VersionTLS12,
	}

	if cfg.Auth.CACertFile != "" {
		pem, err := os.ReadFile(cfg.Auth.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("openstack: reading CA file %s: %w", cfg.Auth.CACertFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("openstack: no certificates found in %s", cfg.Auth.CACertFile)
		}
		tlsCfg.RootCAs = pool
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg
	transport.MaxIdleConns = 1000
	transport.MaxIdleConnsPerHost = 200
	transport.MaxConnsPerHost = 500

	return &http.Client{Transport: transport}, nil
}

// capabilities records what the Neutron on the other end supports, probed once
// at startup rather than guessed per call.
type capabilities struct {
	// TagOnCreate reports whether Neutron accepts tags in a port create body.
	// Older Neutron does not, and Kuryr then has to tag as a second call.
	TagOnCreate bool
	// Trunk reports whether the trunk extension is available, which the
	// nested-vlan driver requires.
	Trunk bool
}

// SupportsTagOnCreate reports whether tags can be set in the create body.
func (c *Client) SupportsTagOnCreate() bool { return c.caps.TagOnCreate }

// SupportsTrunk reports whether the trunk extension is available.
func (c *Client) SupportsTrunk() bool { return c.caps.Trunk }

// ResourceTags returns the tags Kuryr stamps on everything it creates.
//
// It exists as a method, not just the exported field, so that narrow
// interfaces elsewhere can depend on the behaviour rather than on this
// concrete type.
func (c *Client) ResourceTags() []string { return c.Tags }
