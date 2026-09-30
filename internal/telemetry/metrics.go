package telemetry

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds Roundhouse's event metrics. State that can be read on demand
// (sandboxes by state, pool occupancy, the host budget) is exported by a
// collector at scrape time instead, so it costs nothing per event.
//
// Label sets are bounded: start paths, outcomes, and destroy reasons come
// from small fixed sets and routes from the API's own patterns. Sandbox IDs,
// API key names, and anything a caller sends are never labels.
type Metrics struct {
	registry *prometheus.Registry

	Creates          *prometheus.CounterVec
	StartDuration    *prometheus.HistogramVec
	Execs            *prometheus.CounterVec
	ExecDuration     prometheus.Histogram
	Destroys         *prometheus.CounterVec
	TeardownFailures prometheus.Counter
	PoolStarts       *prometheus.CounterVec
	HTTPRequests     *prometheus.CounterVec
	HTTPDuration     *prometheus.HistogramVec
}

// NewMetrics creates and registers the metrics, along with the Go runtime and
// process collectors.
func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		Creates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "roundhouse_sandbox_creates_total",
			Help: "Sandbox creates by start path (pool, restore, cold, or none when it failed first) and outcome.",
		}, []string{"path", "outcome"}),
		StartDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "roundhouse_sandbox_start_seconds",
			Help:    "Time from a create request to a ready sandbox, by start path.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 16), // 0.5ms to about 16s
		}, []string{"path"}),
		Execs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "roundhouse_execs_total",
			Help: "Commands run, by outcome: ok, nonzero, timed_out, start_failed, unresponsive, or error.",
		}, []string{"outcome"}),
		ExecDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "roundhouse_exec_seconds",
			Help:    "Duration of exec calls, from the host's side.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 20), // 0.5ms to about 4.4min
		}),
		Destroys: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "roundhouse_sandbox_destroys_total",
			Help: "Sandboxes destroyed, by reason.",
		}, []string{"reason"}),
		TeardownFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "roundhouse_teardown_failures_total",
			Help: "Teardowns that left resources behind and will be retried.",
		}),
		PoolStarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "roundhouse_pool_starts_total",
			Help: "Sandboxes started to refill warm pools, by outcome.",
		}, []string{"outcome"}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "roundhouse_http_requests_total",
			Help: "API requests by route pattern and status code.",
		}, []string{"route", "code"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "roundhouse_http_request_seconds",
			Help:    "API request duration by route pattern; exec requests last as long as their command.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 20),
		}, []string{"route"}),
	}
	m.registry.MustRegister(
		m.Creates, m.StartDuration, m.Execs, m.ExecDuration, m.Destroys, m.TeardownFailures, m.PoolStarts,
		m.HTTPRequests, m.HTTPDuration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Register adds a scrape-time collector.
func (m *Metrics) Register(c prometheus.Collector) error {
	return m.registry.Register(c)
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObserveCreate records a finished create.
func (m *Metrics) ObserveCreate(path, outcome string, d time.Duration) {
	m.Creates.WithLabelValues(path, outcome).Inc()
	if outcome == "ok" {
		m.StartDuration.WithLabelValues(path).Observe(d.Seconds())
	}
}

// ObserveExec records a finished exec call.
func (m *Metrics) ObserveExec(outcome string, d time.Duration) {
	m.Execs.WithLabelValues(outcome).Inc()
	m.ExecDuration.Observe(d.Seconds())
}
