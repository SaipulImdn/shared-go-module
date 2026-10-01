// Package metrics provides the one standard set of HTTP-request Prometheus
// metrics shared by every service — request counts, latency, response
// codes, and active-connection tracking. Metric NAMES stay per-service
// (via Config.Prefix) since every service is scraped into the same
// Prometheus/Grafana stack and existing dashboards key off e.g.
// "gateway_http_requests_total" / "clockwerk_http_requests_total" — only the
// construction code is now shared, not the metric identity.
package metrics

import (
	"regexp"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// uuidRegex matches UUID segments in URL paths.
var uuidRegex = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// numericRegex matches purely numeric path segments.
var numericRegex = regexp.MustCompile(`/\d+`)

// NormalizePath replaces UUID and numeric ID segments with ":id" so a path
// used as a metric label doesn't blow up Prometheus's label cardinality
// (e.g. "/api/v1/files/<uuid>" and "/api/v1/files/<uuid2>" become the same
// label instead of two, times however many IDs exist).
func NormalizePath(path string) string {
	normalized := uuidRegex.ReplaceAllString(path, ":id")
	normalized = numericRegex.ReplaceAllString(normalized, "/:id")
	return normalized
}

// DefaultBuckets are the histogram bucket boundaries used when Config.Buckets is empty.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Config controls HTTP metrics construction.
type Config struct {
	// Prefix names this service's metrics, e.g. "gateway", "clockwerk",
	// "rubick" — metrics are registered as "<prefix>_http_requests_total" etc.
	// Required.
	Prefix string
	// Buckets overrides the histogram bucket boundaries. Defaults to DefaultBuckets.
	Buckets []float64
	// Registerer overrides where metrics are registered. Defaults to
	// prometheus.DefaultRegisterer. Tests should pass prometheus.NewRegistry()
	// so repeated construction across test cases doesn't panic on duplicate
	// registration against the global default registry.
	Registerer prometheus.Registerer
}

func (c Config) buckets() []float64 {
	if len(c.Buckets) > 0 {
		return c.Buckets
	}
	return DefaultBuckets
}

func (c Config) registerer() prometheus.Registerer {
	if c.Registerer != nil {
		return c.Registerer
	}
	return prometheus.DefaultRegisterer
}

// HTTP holds the standard HTTP-request metric set for one service.
type HTTP struct {
	RequestsTotal     *prometheus.CounterVec
	RequestDuration   *prometheus.HistogramVec
	ResponseCodeTotal *prometheus.CounterVec
	ActiveConnections prometheus.Gauge
}

// NewHTTP registers and returns the standard HTTP metric set for one service.
func NewHTTP(cfg Config) *HTTP {
	factory := promauto.With(cfg.registerer())

	return &HTTP{
		RequestsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: cfg.Prefix + "_http_requests_total",
			Help: "Total number of HTTP requests processed",
		}, []string{"method", "path", "status", "response_code"}),

		RequestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    cfg.Prefix + "_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: cfg.buckets(),
		}, []string{"method", "path"}),

		ResponseCodeTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: cfg.Prefix + "_response_code_total",
			Help: "Total responses by application response code",
		}, []string{"response_code", "path"}),

		ActiveConnections: factory.NewGauge(prometheus.GaugeOpts{
			Name: cfg.Prefix + "_active_connections",
			Help: "Number of active HTTP connections being processed",
		}),
	}
}

// Observe records one completed request across all four metrics in one call.
// path should already be cardinality-reduced (see NormalizePath) — an
// unbounded raw path (containing IDs) would blow up Prometheus's label
// cardinality.
func (h *HTTP) Observe(method, path, status, responseCode string, duration time.Duration) {
	h.RequestsTotal.WithLabelValues(method, path, status, responseCode).Inc()
	h.RequestDuration.WithLabelValues(method, path).Observe(duration.Seconds())
	if responseCode != "" {
		h.ResponseCodeTotal.WithLabelValues(responseCode, path).Inc()
	}
}

// IncActive increments the active-connection gauge. Pair with DecActive via defer.
func (h *HTTP) IncActive() { h.ActiveConnections.Inc() }

// DecActive decrements the active-connection gauge.
func (h *HTTP) DecActive() { h.ActiveConnections.Dec() }
