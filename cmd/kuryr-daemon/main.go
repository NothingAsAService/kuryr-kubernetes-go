// Command kuryr-daemon runs on every node and attaches pod interfaces.
//
// This is the Go replacement for kuryr-daemon. The Python version ran five
// cotyledon-supervised OS processes that shared state through a
// multiprocessing manager and on-disk locks, plus a zombie reaper to clean up
// after them. All of that collapses into one process with goroutines: the
// watchers are controller-runtime reconcilers, the registry is a map, and the
// HTTP server is a goroutine under the same manager.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/cni/registry"
	"opendev.org/openstack/kuryr-kubernetes/pkg/cni/server"
	"opendev.org/openstack/kuryr-kubernetes/pkg/cni/watcher"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
)

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
		fmt.Fprintf(os.Stderr, "kuryr-daemon: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configFile  string
		metricsAddr string
		probeAddr   string
		nodeName    string
	)

	flag.StringVar(&configFile, "config-file", "/etc/kuryr/kuryr.conf", "path to kuryr.conf")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "",
		"address the metrics endpoint binds to; defaults to [prometheus_exporter]cni_exporter_port")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8090",
		"address the health and readiness probes bind to")
	flag.StringVar(&nodeName, "node-name", "",
		"node this daemon serves; defaults to $NODE_NAME then the hostname")

	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	logger := ctrl.Log.WithName("setup")

	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}

	if nodeName == "" {
		nodeName, err = watcher.NodeName()
		if err != nil {
			return err
		}
		if os.Getenv("NODE_NAME") == "" {
			logger.Info("NODE_NAME is not set; falling back to the hostname. "+
				"If the node is registered under a different name this daemon will see no pods",
				"hostname", nodeName)
		}
	}

	if metricsAddr == "" {
		metricsAddr = fmt.Sprintf(":%d", cfg.Prometheus.CNIPort)
	}

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("building Kubernetes client config: %w", err)
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: metricsAddr},
		// Every node runs one daemon; there is nothing to elect.
		LeaderElection:         false,
		HealthProbeBindAddress: probeAddr,
		Cache:                  watcher.CacheOptions(nodeName),
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	reg := registry.New()
	metrics := server.NewMetrics(ctrlmetrics.Registry, reg)
	srv := server.New(cfg, reg, ctrl.Log.WithName("cni"), metrics)

	if err := (&watcher.KuryrPortReconciler{Client: mgr.GetClient(), Registry: reg}).
		SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the KuryrPort watcher: %w", err)
	}
	if err := (&watcher.PodReconciler{Client: mgr.GetClient(), Registry: reg}).
		SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the Pod watcher: %w", err)
	}

	if err := mgr.Add(cniServerRunnable{srv}); err != nil {
		return fmt.Errorf("registering the CNI server: %w", err)
	}

	if err := mgr.AddHealthzCheck("cni", func(*http.Request) error {
		if !srv.Healthy() {
			return fmt.Errorf("too many consecutive CNI failures")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("registering health check: %w", err)
	}
	// Readiness waits for the informers, so kubelet does not start sending
	// CNI requests the daemon has no data to answer.
	if err := mgr.AddReadyzCheck("cache", func(*http.Request) error {
		if !mgr.GetCache().WaitForCacheSync(context.Background()) {
			return fmt.Errorf("informer cache has not synced")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("registering readiness check: %w", err)
	}

	logger.Info("starting kuryr-daemon",
		"node", nodeName,
		"bindAddress", cfg.CNIDaemon.BindAddress,
		"metrics", metricsAddr)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("running manager: %w", err)
	}
	return nil
}

// cniServerRunnable lets the manager own the HTTP server lifecycle.
type cniServerRunnable struct{ srv *server.Server }

func (r cniServerRunnable) Start(ctx context.Context) error { return r.srv.Start(ctx) }

// NeedLeaderElection is false: every node serves its own CNI requests.
func (r cniServerRunnable) NeedLeaderElection() bool { return false }

var _ manager.Runnable = cniServerRunnable{}
var _ manager.LeaderElectionRunnable = cniServerRunnable{}
