package server

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"opendev.org/openstack/kuryr-kubernetes/pkg/cni/registry"
)

// Metrics mirrors what kuryr_kubernetes/cni/prometheus_exporter.py exported,
// so existing dashboards keep working.
//
// The Python daemon had to ship these across a process boundary through a
// multiprocessing queue and a dedicated exporter process. In one process the
// collector is just a package-level registry.
type Metrics struct {
	duration *prometheus.HistogramVec
	failures *prometheus.CounterVec
	tracked  prometheus.GaugeFunc
}

// NewMetrics registers the collectors.
//
// The registry is passed so the tracked-pod gauge can read it when it is
// scraped rather than being pushed on every change. That gauge is the one
// that says whether the daemon's view of the node has drifted: it should
// track the number of pods on the node, and a number that only ever grows is
// the symptom of entries that are recorded but never removed.
func NewMetrics(reg prometheus.Registerer, pods *registry.Registry) *Metrics {
	m := &Metrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kuryr_cni_request_duration_seconds",
			Help: "Time a CNI request took, by command and outcome.",
			// A CNI ADD blocks on Neutron activating the port, so the
			// interesting range runs from milliseconds to a minute.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120},
		}, []string{"command", "outcome"}),

		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kuryr_cni_request_failures_total",
			Help: "CNI requests that failed, by command and reason.",
		}, []string{"command", "reason"}),
	}
	if pods != nil {
		m.tracked = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "kuryr_cni_tracked_pods",
			Help: "Pods this node's daemon currently holds binding information for.",
		}, func() float64 { return float64(pods.Len()) })
		reg.MustRegister(m.tracked)
	}

	reg.MustRegister(m.duration, m.failures)
	return m
}

// Observe records the outcome of one request.
func (m *Metrics) Observe(command string, err error, d time.Duration) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
		m.failures.WithLabelValues(command, reasonFor(err)).Inc()
	}
	m.duration.WithLabelValues(command, outcome).Observe(d.Seconds())
}

// reasonFor buckets errors into a small, bounded label set. Using the error
// text would let an unbounded number of label values into the metric.
func reasonFor(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, registry.ErrPodGone):
		return "pod_gone"
	case errors.Is(err, registry.ErrTimeout):
		return "timeout"
	case isUIDMismatch(err):
		return "uid_mismatch"
	default:
		return "internal"
	}
}
