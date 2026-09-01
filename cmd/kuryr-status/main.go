// Command kuryr-status checks that a deployment is in a state Kuryr can run
// in, before it is asked to.
//
// This replaces kuryr-k8s-status and kuryr-k8s-sanity, which were two commands
// for one job: telling an operator what is wrong while there is still time to
// fix it, rather than after the controller has entered a crash loop or the
// first pod has hung in ContainerCreating.
//
// The upstream status command's only check was a Stein-era migration from pod
// annotations to CRDs, which never applies here: this implementation has used
// CRDs from its first commit. What it checks instead is what this
// implementation can actually get wrong -- a configuration whose parts
// contradict each other, a cloud missing an extension a driver needs, or CRDs
// that were never installed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
)

// Exit codes, and the words for them, are the ones kuryr-k8s-status used, so
// a script that wrapped it keeps working.
const (
	codeSuccess = 0
	codeWarning = 1
	codeFailure = 2
)

var codeNames = map[int]string{
	codeSuccess: "Success",
	codeWarning: "Warning",
	codeFailure: "Failure",
}

// result is one check's verdict. Details are only expected on a warning or a
// failure, and should say what to do about it rather than only what is wrong.
type result struct {
	name    string
	code    int
	details string
}

func ok(name string) result { return result{name: name, code: codeSuccess} }

func warn(name, format string, args ...any) result {
	return result{name: name, code: codeWarning, details: fmt.Sprintf(format, args...)}
}

func fail(name, format string, args ...any) result {
	return result{name: name, code: codeFailure, details: fmt.Sprintf(format, args...)}
}

func main() {
	var (
		configFile string
		configOnly bool
	)
	flag.StringVar(&configFile, "config-file", "/etc/kuryr/kuryr.conf", "path to kuryr.conf")
	flag.BoolVar(&configOnly, "config-only", false,
		"check the configuration alone, without contacting OpenStack or Kubernetes")
	flag.Parse()

	// The checks below log nothing themselves; silencing controller-runtime
	// keeps its startup chatter out of what is meant to be a readable report.
	ctrl.SetLogger(logr.Discard())

	results := run(configFile, configOnly)
	report(results)
	os.Exit(worst(results))
}

func run(configFile string, configOnly bool) []result {
	cfg, err := config.Load(configFile)
	if err != nil {
		// Nothing else can be judged without it, so this is the whole report.
		return []result{fail("Configuration", "%s does not load: %v", configFile, err)}
	}

	results := []result{ok("Configuration"), checkPoolSizing(cfg)}
	if configOnly {
		return results
	}

	ctx := context.Background()
	results = append(results, checkKubernetes())
	return append(results, checkOpenStack(ctx, cfg)...)
}

// checkPoolSizing reports pool settings that load without error but will not
// behave the way the numbers suggest.
//
// Config validation already rejects the combinations that cannot work at all;
// these are the ones that work badly, which is why they are warnings.
func checkPoolSizing(cfg *config.Config) result {
	const name = "Ports pool sizing"

	if cfg.Kubernetes.VIFPoolDriver == "noop" {
		return ok(name)
	}

	p := cfg.VIFPool
	switch {
	case p.PortsPoolBatch < p.PortsPoolMin:
		return warn(name,
			"ports_pool_batch (%d) is smaller than ports_pool_min (%d), so one refill "+
				"never reaches the minimum and every pod start triggers another; "+
				"raise ports_pool_batch to at least ports_pool_min",
			p.PortsPoolBatch, p.PortsPoolMin)
	case p.PortsPoolMax != 0 && p.PortsPoolBatch > p.PortsPoolMax:
		return warn(name,
			"ports_pool_batch (%d) is larger than ports_pool_max (%d), so a refill would "+
				"overshoot the ceiling and the surplus is deleted again; "+
				"lower ports_pool_batch or raise ports_pool_max",
			p.PortsPoolBatch, p.PortsPoolMax)
	}
	return ok(name)
}

// checkKubernetes reports whether the CRDs Kuryr stores its state in are
// served by the API server. Without them every controller fails to start its
// informer cache, which presents as a crash loop with no obvious cause.
func checkKubernetes() result {
	const name = "Kuryr CRDs"

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fail(name, "no Kubernetes configuration: %v", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return fail(name, "building a discovery client: %v", err)
	}

	gv := kuryrv1.GroupVersion.String()
	list, err := dc.ServerResourcesForGroupVersion(gv)
	if err != nil {
		return fail(name, "the API server does not serve %s (%v); apply config/crd", gv, err)
	}

	served := map[string]bool{}
	for _, r := range list.APIResources {
		served[r.Kind] = true
	}

	var missing []string
	for _, kind := range []string{"KuryrPort", "KuryrNetwork", "KuryrNetworkPolicy", "KuryrLoadBalancer"} {
		if !served[kind] {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		return fail(name, "%s is served but %s missing; re-apply config/crd",
			gv, strings.Join(missing, ", "))
	}
	return ok(name)
}

// checkOpenStack reports on the cloud, and on the endpoints and extensions the
// configured drivers need from it.
func checkOpenStack(ctx context.Context, cfg *config.Config) []result {
	const name = "OpenStack connection"

	osClient, err := openstack.New(ctx, cfg)
	if err != nil {
		// Every remaining check needs this connection, so there is nothing to
		// add after it.
		return []result{fail(name, "%v", err)}
	}

	results := []result{ok(name), checkTrunkExtension(cfg, osClient)}

	const octavia = "Octavia endpoint"
	switch {
	case !cfg.HandlerEnabled("service"):
		results = append(results, ok(octavia))
	case osClient.LoadBalancer == nil:
		results = append(results, fail(octavia,
			"the service handler is enabled but Octavia is not in the service catalog; "+
				"Services would never get a load balancer"))
	default:
		results = append(results, ok(octavia))
	}

	const nova = "Nova endpoint"
	switch {
	case cfg.Kubernetes.PodVIFDriver != "nested-dpdk":
		results = append(results, ok(nova))
	case osClient.Compute == nil:
		results = append(results, fail(nova,
			"pod_vif_driver=nested-dpdk attaches interfaces through Nova, "+
				"which is not in the service catalog"))
	default:
		results = append(results, ok(nova))
	}

	const tags = "Neutron resource tags"
	if len(cfg.NeutronDefaults.ResourceTags) > 0 && !osClient.SupportsTagOnCreate() {
		results = append(results, warn(tags,
			"resource_tags is set but this Neutron does not support tagging on create, "+
				"so every resource costs a second call to tag it"))
	} else {
		results = append(results, ok(tags))
	}

	return results
}

func checkTrunkExtension(cfg *config.Config, osClient *openstack.Client) result {
	const name = "Neutron trunk extension"

	if cfg.Kubernetes.PodVIFDriver != "nested-vlan" {
		return ok(name)
	}
	if !osClient.SupportsTrunk() {
		return fail(name,
			"pod_vif_driver=nested-vlan rides pod ports on the node VM trunk, "+
				"but this Neutron does not expose the trunk extension")
	}
	return ok(name)
}

// report prints one line per check, then the details of anything that was not
// a success. Keeping the summary dense and the explanations below it means the
// common case -- everything passing -- is one screen.
func report(results []result) {
	width := 0
	for _, r := range results {
		if len(r.name) > width {
			width = len(r.name)
		}
	}

	fmt.Println("Upgrade check results")
	fmt.Println(strings.Repeat("-", width+13))
	for _, r := range results {
		fmt.Printf("%-*s  %s\n", width, r.name, codeNames[r.code])
	}

	for _, r := range results {
		if r.details == "" {
			continue
		}
		fmt.Printf("\n%s: %s\n  %s\n", codeNames[r.code], r.name, r.details)
	}
}

func worst(results []result) int {
	code := codeSuccess
	for _, r := range results {
		if r.code > code {
			code = r.code
		}
	}
	return code
}
