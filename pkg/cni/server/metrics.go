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
}

// NewMetrics registers the collectors.
func NewMetrics(reg prometheus.Registerer) *Metrics {
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
