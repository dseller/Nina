// Package observ holds the gateway's instrumentation.
package observ

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the gateway's Prometheus instrumentation.
//
// Every label used here is bounded: routes are labelled by their gateway path
// template and operation id, never by the concrete request path, so a client
// cannot blow up cardinality by varying an id.
type Metrics struct {
	Requests         *prometheus.CounterVec
	Duration         *prometheus.HistogramVec
	InFlight         prometheus.Gauge
	UpstreamDuration *prometheus.HistogramVec
	UpstreamErrors   *prometheus.CounterVec
	UpstreamAttempts *prometheus.HistogramVec
	BreakerState     *prometheus.GaugeVec
	HostHealthy      *prometheus.GaugeVec

	AuthTotal       *prometheus.CounterVec
	RateLimitTotal  *prometheus.CounterVec
	ValidationTotal *prometheus.CounterVec
	CacheTotal      *prometheus.CounterVec

	Reloads     *prometheus.CounterVec
	SpecFetches *prometheus.CounterVec
	Routes      prometheus.Gauge
	BuildInfo   *prometheus.GaugeVec
}

// New registers the metric set with r.
func New(r prometheus.Registerer, version string) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_requests_total",
			Help: "Requests handled, by route and response status.",
		}, []string{"route", "method", "status"}),

		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nina_request_duration_seconds",
			Help:    "End-to-end gateway latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),

		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "nina_requests_in_flight",
			Help: "Requests currently being served.",
		}),

		UpstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nina_upstream_duration_seconds",
			Help:    "Time spent waiting on the upstream.",
			Buckets: prometheus.DefBuckets,
		}, []string{"backend"}),

		UpstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_upstream_errors_total",
			Help: "Upstream failures by kind.",
		}, []string{"backend", "kind"}),

		UpstreamAttempts: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nina_upstream_attempts",
			Help:    "Attempts made per request, including the first.",
			Buckets: []float64{1, 2, 3, 4, 5},
		}, []string{"backend"}),

		BreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nina_circuit_breaker_state",
			Help: "Circuit breaker state (0 closed, 1 open, 2 half-open).",
		}, []string{"backend"}),

		HostHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nina_upstream_host_healthy",
			Help: "1 when an upstream host is passing its health check.",
		}, []string{"backend", "host"}),

		AuthTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_auth_total",
			Help: "Authentication outcomes.",
		}, []string{"middleware", "result"}),

		RateLimitTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_ratelimit_total",
			Help: "Rate limit decisions.",
		}, []string{"middleware", "decision"}),

		ValidationTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_validation_total",
			Help: "Request validation outcomes.",
		}, []string{"route", "result"}),

		CacheTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_cache_total",
			Help: "Response cache outcomes.",
		}, []string{"route", "result"}),

		Reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_reloads_total",
			Help: "Configuration reloads by outcome.",
		}, []string{"result"}),

		SpecFetches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_spec_fetches_total",
			Help: "Upstream spec fetches by backend and outcome.",
		}, []string{"backend", "result"}),

		Routes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "nina_routes",
			Help: "Routes currently served.",
		}),

		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nina_build_info",
			Help: "Build information; always 1.",
		}, []string{"version"}),
	}

	r.MustRegister(
		m.Requests, m.Duration, m.InFlight, m.UpstreamDuration, m.UpstreamErrors,
		m.UpstreamAttempts, m.BreakerState, m.HostHealthy, m.AuthTotal,
		m.RateLimitTotal, m.ValidationTotal, m.CacheTotal, m.Reloads,
		m.SpecFetches, m.Routes, m.BuildInfo,
	)
	m.BuildInfo.WithLabelValues(version).Set(1)
	return m
}

// The following satisfy chain.Metrics, which keeps internal/mw free of a
// Prometheus dependency.

func (m *Metrics) CountAuth(middleware, result string) {
	m.AuthTotal.WithLabelValues(middleware, result).Inc()
}

func (m *Metrics) CountRateLimit(middleware, decision string) {
	m.RateLimitTotal.WithLabelValues(middleware, decision).Inc()
}

func (m *Metrics) CountValidation(route, result string) {
	m.ValidationTotal.WithLabelValues(route, result).Inc()
}

func (m *Metrics) CountCache(route, result string) {
	m.CacheTotal.WithLabelValues(route, result).Inc()
}
