// Command kuryr-controller keeps OpenStack networking in step with the
// Kubernetes objects Kuryr manages.
//
// This is the Go replacement for kuryr-k8s-controller. The most visible
// difference is what is no longer here: there is no Watcher, no Async queue,
// no Retry decorator, no leader-elector sidecar and no periodic task polling
// it. controller-runtime supplies all of that, including active/passive HA
// through a Lease object, so the process can simply declare what it wants
// reconciled.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/internal/controller"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/policy"
	"opendev.org/openstack/kuryr-kubernetes/pkg/pool"
)

// leaderElectionID is the Lease that arbitrates which replica is active.
// Kuryr is active/passive by necessity: two controllers reconciling the same
// pod would both create Neutron ports and only one would win the status
// write, leaking the other.
const leaderElectionID = "kuryr-controller.openstack.org"

var scheme = runtime.NewScheme()

func init() {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := kuryrv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "kuryr-controller: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configFile    string
		metricsAddr   string
		probeAddr     string
		leaderElect   bool
		namespace     string
		secureMetrics bool
		enableHTTP2   bool
	)

	flag.StringVar(&configFile, "config-file", "/etc/kuryr/kuryr.conf",
		"path to kuryr.conf")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "",
		"address the metrics endpoint binds to; defaults to [prometheus_exporter]controller_exporter_port")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"address the health and readiness probes bind to")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"enable active/passive HA; overrides [kubernetes]controller_ha when set")
	flag.StringVar(&namespace, "leader-election-namespace", "kube-system",
		"namespace holding the leader election Lease")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"serve metrics over HTTPS and require the caller to be authenticated and authorised")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"enable HTTP/2 on the metrics server; off by default, see CVE-2023-44487 and CVE-2023-39325")

	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	logger := ctrl.Log.WithName("setup")

	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}
	logger.Info("configuration loaded",
		"file", configFile,
		"handlers", cfg.Kubernetes.EnabledHandlers,
		"podVIFDriver", cfg.Kubernetes.PodVIFDriver,
		"subnetDriver", cfg.Kubernetes.PodSubnetsDriver,
		"poolDriver", cfg.Kubernetes.VIFPoolDriver)

	if metricsAddr == "" {
		metricsAddr = fmt.Sprintf(":%d", cfg.Prometheus.ControllerPort)
	}
	// The flag is an override; without it the config file decides.
	if !leaderElect {
		leaderElect = cfg.Kubernetes.ControllerHA
	}

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("building Kubernetes client config: %w", err)
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsOptions(metricsAddr, secureMetrics, enableHTTP2),
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: namespace,
		// Release the lease on a clean shutdown so a rolling update fails
		// over in milliseconds rather than waiting for the lease to expire.
		LeaderElectionReleaseOnCancel: true,
		Cache:                         cacheOptions(cfg),
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	ctx := ctrl.SetupSignalHandler()

	osClient, err := openstack.New(ctx, cfg)
	if err != nil {
		return err
	}
	logger.Info("connected to OpenStack",
		"trunkSupport", osClient.SupportsTrunk(),
		"tagOnCreate", osClient.SupportsTagOnCreate(),
		"octavia", osClient.LoadBalancer != nil)

	if err := setupControllers(ctx, mgr, cfg, osClient); err != nil {
		return err
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("registering health check: %w", err)
	}
	// Readiness reports on the OpenStack side too. A controller that cannot
	// reach Neutron cannot create pods, and saying so here is what stops a
	// rollout from proceeding into an outage.
	if err := mgr.AddReadyzCheck("openstack", openstackReadyCheck(osClient)); err != nil {
		return fmt.Errorf("registering readiness check: %w", err)
	}

	logger.Info("starting kuryr-controller", "leaderElection", leaderElect, "metrics", metricsAddr)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running manager: %w", err)
	}
	return nil
}

// setupControllers wires the drivers named in kuryr.conf into the reconcilers
// the enabled handlers require.
func setupControllers(ctx context.Context, mgr manager.Manager, cfg *config.Config, osClient *openstack.Client) error {
	deps := drivers.Deps{Config: cfg, OpenStack: osClient, K8s: mgr.GetClient()}

	pools, err := setupPodControllers(mgr, cfg, deps)
	if err != nil {
		return err
	}
	if err := setupNamespaceControllers(mgr, cfg, deps, osClient, pools); err != nil {
		return err
	}
	if err := setupPolicyControllers(mgr, cfg, osClient, deps); err != nil {
		return err
	}
	return setupServiceControllers(ctx, mgr, cfg, deps, osClient)
}

// setupPodControllers wires the pod lifeline: Pod, KuryrPort and the drivers
// that turn one into a Neutron port.
func setupPodControllers(mgr manager.Manager, cfg *config.Config, deps drivers.Deps) (controller.Pools, error) {
	// The vif and kuryrport handlers are two halves of one feature; config
	// validation has already rejected enabling only one of them.
	if !cfg.HandlerEnabled("vif") {
		ctrl.Log.WithName("setup").Info(
			"pod networking handlers are disabled; no pod controllers will run")
		return nil, nil
	}

	projectDrv, err := drivers.NewProjectDriver(cfg.Kubernetes.PodProjectDriver, deps)
	if err != nil {
		return nil, err
	}
	subnetDrv, err := drivers.NewSubnetDriver(cfg.Kubernetes.PodSubnetsDriver, deps)
	if err != nil {
		return nil, err
	}
	sgDrv, err := drivers.NewSecurityGroupsDriver(cfg.Kubernetes.PodSecurityGroupsDriver, deps)
	if err != nil {
		return nil, err
	}
	vifDrv, pools, err := buildVIFDriver(mgr, cfg, deps)
	if err != nil {
		return nil, err
	}

	// Extra interfaces go through the same driver as the default one when the
	// NetworkAttachmentDefinition names no alias, so a pooled deployment
	// pools them too.
	multiVIF, err := drivers.NewMultiVIFDrivers(cfg.Kubernetes.MultiVIFDrivers, deps,
		func(alias string) (drivers.PodVIFDriver, error) {
			if alias == "" {
				return vifDrv, nil
			}
			return drivers.NewPodVIFDriver(alias, deps)
		})
	if err != nil {
		return nil, err
	}

	podReconciler := &controller.PodReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: cfg,
	}
	if err := podReconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("setting up the pod controller: %w", err)
	}

	kpReconciler := &controller.KuryrPortReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Config:   cfg,
		Project:  projectDrv,
		Subnets:  subnetDrv,
		SGs:      sgDrv,
		VIF:      vifDrv,
		MultiVIF: multiVIF,
	}
	if err := kpReconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("setting up the kuryrport controller: %w", err)
	}

	return pools, nil
}

// setupNamespaceControllers wires per-namespace networks.
//
// The namespace handler decides a namespace needs a network; the kuryrnetwork
// handler builds it in Neutron. Enabling one without the other leaves either
// requests nobody serves or resources nobody asks for, which config
// validation already rejects.
func setupNamespaceControllers(mgr manager.Manager, cfg *config.Config, deps drivers.Deps,
	osClient *openstack.Client, pools controller.Pools) error {

	if !cfg.HandlerEnabled("namespace") {
		return nil
	}

	projectDrv, err := drivers.NewProjectDriver(cfg.Kubernetes.NamespaceProjectDriver, deps)
	if err != nil {
		return err
	}

	nsReconciler := &controller.NamespaceReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Config:  cfg,
		Project: projectDrv,
	}
	if err := nsReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the namespace controller: %w", err)
	}

	knReconciler := &controller.KuryrNetworkReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
		OpenStack: osClient,
		Pool:      pools,
	}
	if err := knReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the kuryrnetwork controller: %w", err)
	}

	return nil
}

// setupPolicyControllers wires NetworkPolicy support.
//
// Nothing here needs the pod drivers: the first controller only reads the
// cluster, and the second only talks to Neutron. What connects them to a
// running pod is the policy security groups driver, which the kuryrport
// controller uses.
func setupPolicyControllers(mgr manager.Manager, cfg *config.Config,
	osClient *openstack.Client, deps drivers.Deps) error {

	if !cfg.HandlerEnabled("policy") {
		return nil
	}

	npReconciler := &controller.NetworkPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: cfg,
		Resolver: &policy.ClientResolver{
			Client: mgr.GetClient(),
			// A namespaceSelector collapses to one rule per namespace only
			// when namespaces actually have their own subnets.
			NamespaceSubnets: cfg.Kubernetes.PodSubnetsDriver == "namespace",
		},
	}
	if err := npReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the networkpolicy controller: %w", err)
	}

	npProjectDrv, err := drivers.NewProjectDriver(cfg.Kubernetes.NetworkPolicyProjectDriver, deps)
	if err != nil {
		return err
	}

	knpReconciler := &controller.KuryrNetworkPolicyReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
		OpenStack: osClient,
		Project:   npProjectDrv,
	}
	if err := knpReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the kuryrnetworkpolicy controller: %w", err)
	}

	return nil
}

// setupServiceControllers wires Service support onto Octavia.
//
// The service handler turns a Service and its EndpointSlices into a
// KuryrLoadBalancer; the kuryrloadbalancer handler builds the Octavia
// topology. The Python "endpoints" handler has no counterpart here: reading
// EndpointSlices is part of building the spec, which is one controller's job
// rather than two.
func setupServiceControllers(ctx context.Context, mgr manager.Manager, cfg *config.Config, deps drivers.Deps,
	osClient *openstack.Client) error {

	if !cfg.HandlerEnabled("service") {
		return nil
	}
	logger := ctrl.Log.WithName("setup")

	if !osClient.HasOctavia() {
		return fmt.Errorf("the service handler is enabled but %w", openstack.ErrNoOctavia)
	}

	projectDrv, err := drivers.NewProjectDriver(cfg.Kubernetes.ServiceProjectDriver, deps)
	if err != nil {
		return err
	}
	subnetDrv, err := drivers.NewServiceSubnetDriver(cfg.Kubernetes.ServiceSubnetsDriver, deps)
	if err != nil {
		return err
	}
	sgDrv, err := drivers.NewServiceSecurityGroupsDriver(cfg.Kubernetes.ServiceSecurityGroupsDriver, deps)
	if err != nil {
		return err
	}

	provider, err := octaviaProvider(ctx, cfg, osClient)
	if err != nil {
		return err
	}

	svcReconciler := &controller.ServiceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Config:   cfg,
		Project:  projectDrv,
		Subnets:  subnetDrv,
		SGs:      sgDrv,
		Provider: provider,
	}
	if err := svcReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the service controller: %w", err)
	}

	// The floating IP driver is optional: a cluster with no external network
	// can still serve ClusterIP Services, and only a LoadBalancer Service
	// needs one. Building it lazily here means the missing configuration is
	// reported against the Service that needs it, not at startup.
	var publicIP drivers.PublicIPDriver
	if cfg.NeutronDefaults.ExternalSvcNet != "" {
		publicIP, err = drivers.NewFloatingIPDriver(deps)
		if err != nil {
			return err
		}
	} else {
		logger.Info("[neutron_defaults]external_svc_net is unset; " +
			"Services of type LoadBalancer will be reported as unsupported")
	}

	// L2 member mode needs to know which subnet each backend pod sits on,
	// which is the pod subnet driver's job. In the default L3 mode the load
	// balancer reaches members through a router and these are never
	// consulted, so they are not built: a Services-only deployment has no
	// reason to require the pod networking configuration they need.
	var podProject drivers.ProjectDriver
	var podSubnets drivers.SubnetDriver
	if cfg.OctaviaDefaults.MemberMode == "L2" {
		podProject, err = drivers.NewProjectDriver(cfg.Kubernetes.PodProjectDriver, deps)
		if err != nil {
			return err
		}
		podSubnets, err = drivers.NewSubnetDriver(cfg.Kubernetes.PodSubnetsDriver, deps)
		if err != nil {
			return err
		}
	}

	klbReconciler := &controller.KuryrLoadBalancerReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Config:     cfg,
		OpenStack:  osClient,
		PublicIP:   publicIP,
		PodProject: podProject,
		PodSubnets: podSubnets,
	}
	if err := klbReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the kuryrloadbalancer controller: %w", err)
	}

	return nil
}

// octaviaProvider resolves the configured Octavia provider, checking it
// exists.
//
// Octavia only rejects an unknown provider when a load balancer is created,
// and the message does not say what went wrong. Checking once at startup
// turns a per-Service mystery into a clear refusal to start.
func octaviaProvider(ctx context.Context, cfg *config.Config, osClient *openstack.Client) (string, error) {
	wanted := cfg.Kubernetes.EndpointsOctaviaProvider
	if wanted == "" || wanted == "default" {
		// Let the cloud pick.
		return "", nil
	}

	available, err := osClient.ListProviders(ctx)
	if err != nil {
		return "", fmt.Errorf("checking that the Octavia provider %q exists: %w", wanted, err)
	}
	for _, name := range available {
		if name == wanted {
			return wanted, nil
		}
	}
	return "", fmt.Errorf("[kubernetes]endpoints_driver_octavia_provider is %q, "+
		"but this cloud offers %v", wanted, available)
}

// cacheOptions keeps the informer cache to what the enabled handlers need.
//
// The Python controller watched whole collections regardless. On a large
// cluster the pod cache dominates the controller memory footprint, so it is
// worth being deliberate about what is held.
func cacheOptions(cfg *config.Config) cache.Options {
	opts := cache.Options{SyncPeriod: &cfg.Kubernetes.ResyncPeriod}
	return opts
}

// openstackReadyCheck reports unready while Neutron is unreachable.
func openstackReadyCheck(c *openstack.Client) healthz.Checker {
	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		// A cheap authenticated call: if the catalog and token are good this
		// returns quickly, and if Keystone or Neutron are down it does not.
		if _, err := c.ListPorts(ctx, openstack.PortFilter{Name: "__kuryr_readiness_probe__"}); err != nil {
			return fmt.Errorf("neutron is not reachable: %w", err)
		}
		return nil
	}
}

// buildVIFDriver assembles the pod VIF driver, wrapping it in a ports pool
// when one is configured.
//
// The pool is a decorator, so nothing downstream changes: the reconciler is
// handed a drivers.PodVIFDriver either way and never learns whether its ports
// came from a pool or straight from Neutron.
func buildVIFDriver(mgr manager.Manager, cfg *config.Config,
	deps drivers.Deps) (drivers.PodVIFDriver, controller.Pools, error) {

	logger := ctrl.Log.WithName("setup")

	if cfg.Kubernetes.VIFPoolDriver == "noop" {
		drv, err := drivers.NewPodVIFDriver(cfg.Kubernetes.PodVIFDriver, deps)
		if err != nil {
			return nil, nil, err
		}
		logger.Info("ports pool disabled", "podVIFDriver", drv.Name())
		// With no pool there is nothing for a network teardown to empty.
		return drv, nil, nil
	}

	metrics := pool.NewMetrics(ctrlmetrics.Registry)

	var pools pool.Manager
	if mapping := cfg.VIFPool.VIFPoolMapping; len(mapping) > 1 {
		// A mixed cluster: some nodes bare metal, some node VMs. Each needs
		// its own driver and its own pool, chosen per node at request time.
		inner := make(map[string]drivers.PodVIFDriver, len(mapping))
		for podDriver := range mapping {
			drv, err := drivers.NewPodVIFDriver(podDriver, deps)
			if err != nil {
				return nil, nil, fmt.Errorf("building VIF driver %q for the multi pool: %w", podDriver, err)
			}
			inner[podDriver] = drv
		}
		m, err := pool.NewMulti(cfg, deps.OpenStack, deps.K8s, inner, metrics)
		if err != nil {
			return nil, nil, err
		}
		pools = m
	} else {
		drv, err := drivers.NewPodVIFDriver(cfg.Kubernetes.PodVIFDriver, deps)
		if err != nil {
			return nil, nil, err
		}
		p, err := pool.New(cfg, deps.OpenStack, deps.K8s, drv, metrics)
		if err != nil {
			return nil, nil, err
		}
		pools = p
	}

	if err := mgr.Add(poolRunnable{pools}); err != nil {
		return nil, nil, fmt.Errorf("registering the ports pool: %w", err)
	}

	logger.Info("ports pool enabled",
		"driver", pools.Name(),
		"min", cfg.VIFPool.PortsPoolMin,
		"max", cfg.VIFPool.PortsPoolMax,
		"batch", cfg.VIFPool.PortsPoolBatch,
		"recycleEvery", cfg.VIFPool.PortsPoolUpdateFrequency)

	return pools, pools, nil
}

// poolRunnable recovers the pools once the manager is leading, then runs the
// recycler for as long as it keeps leading.
//
// Recovery has to happen under leader election: it lists and deletes Neutron
// ports, and two controllers doing that at once would fight. Until it
// finishes the pool returns a retryable error, so the reconcilers simply
// requeue rather than needing to be sequenced.
type poolRunnable struct{ pool pool.Manager }

func (r poolRunnable) Start(ctx context.Context) error {
	if err := r.pool.Recover(ctx); err != nil {
		// Fatal: serving pods from a half-recovered pool would duplicate
		// ports that already exist in Neutron.
		return fmt.Errorf("recovering ports pools: %w", err)
	}
	return r.pool.RunRecycler(ctx)
}

func (r poolRunnable) NeedLeaderElection() bool { return true }

var _ manager.Runnable = poolRunnable{}
var _ manager.LeaderElectionRunnable = poolRunnable{}

// metricsOptions configures the metrics endpoint.
//
// Serving it over TLS and checking the caller is what replaced the
// kube-rbac-proxy sidecar: the manager verifies the bearer token and asks the
// API server whether that identity may read metrics, so no extra container and
// no extra port is needed. config/rbac/metrics_auth_role.yaml grants the two
// APIs that requires, and metrics_reader_role.yaml is what an operator binds
// their scrape agent to.
//
// HTTP/2 is off unless asked for. The metrics endpoint is scraped by one
// agent every thirty seconds, so multiplexing buys nothing, and leaving it on
// carries the stream-reset denial of service of CVE-2023-44487 and
// CVE-2023-39325 for no gain.
func metricsOptions(addr string, secure, http2 bool) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   addr,
		SecureServing: secure,
	}
	if !http2 {
		opts.TLSOpts = append(opts.TLSOpts, func(c *tls.Config) {
			c.NextProtos = []string{"http/1.1"}
		})
	}
	if secure {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	return opts
}
