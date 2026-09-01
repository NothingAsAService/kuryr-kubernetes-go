package pool

import "github.com/prometheus/client_golang/prometheus"

// driverLabel names the wrapped VIF driver on every pool counter, so one
// dashboard can tell a nested pool from a baremetal one.
const driverLabel = "driver"

// Metrics exposes what an operator needs to tell a healthy pool from one that
// is silently failing to refill.
//
// The Python implementation exported almost nothing here, which made "pods are
// slow to start" hard to attribute: it could be Neutron, the pool being empty,
// or repopulation failing. These separate those three.
type Metrics struct {
	size        *prometheus.GaugeVec
	createdPort *prometheus.CounterVec
	reusedPort  *prometheus.CounterVec
	recycledPrt *prometheus.CounterVec
	deletedPort *prometheus.CounterVec
	populateErr *prometheus.CounterVec
}

// NewMetrics registers the pool collectors.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		size: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kuryr_ports_pool_size",
			Help: "Free ports currently held in a pool.",
		}, []string{"host", "project", "network"}),

		createdPort: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kuryr_ports_pool_created_total",
			Help: "Ports created to fill pools.",
		}, []string{driverLabel}),

		reusedPort: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kuryr_ports_pool_reused_total",
			Help: "Ports served to a pod from a pool rather than newly created.",
		}, []string{driverLabel}),

		recycledPrt: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kuryr_ports_pool_recycled_total",
			Help: "Ports handed back by a deleted pod for reuse.",
		}, []string{driverLabel}),

		deletedPort: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kuryr_ports_pool_deleted_total",
			Help: "Ports deleted because their pool was already at ports_pool_max.",
		}, []string{driverLabel}),

		populateErr: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kuryr_ports_pool_populate_failures_total",
			Help: "Failed attempts to refill a pool. A rising count with an empty pool means pods are waiting on Neutron.",
		}, []string{driverLabel}),
	}
	reg.MustRegister(m.size, m.createdPort, m.reusedPort, m.recycledPrt, m.deletedPort, m.populateErr)
	return m
}

func (m *Metrics) observePoolSize(k key, size int) {
	if m == nil {
		return
	}
	m.size.WithLabelValues(k.Host, k.ProjectID, k.NetworkID).Set(float64(size))
}

func (m *Metrics) created(driver string, n int) {
	if m == nil || n == 0 {
		return
	}
	m.createdPort.WithLabelValues(driver).Add(float64(n))
}

func (m *Metrics) reuse(driver string) {
	if m == nil {
		return
	}
	m.reusedPort.WithLabelValues(driver).Inc()
}

func (m *Metrics) recycled(driver string) {
	if m == nil {
		return
	}
	m.recycledPrt.WithLabelValues(driver).Inc()
}

func (m *Metrics) deleted(driver string) {
	if m == nil {
		return
	}
	m.deletedPort.WithLabelValues(driver).Inc()
}

func (m *Metrics) populateFailure(driver string) {
	if m == nil {
		return
	}
	m.populateErr.WithLabelValues(driver).Inc()
}
