// Package observ holds the gateway's instrumentation.
package observ

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the gateway's Prometheus instrumentation.
//
// The set is small on purpose. Per-request detail is on the access log line
// and point-in-time state is on the admin /status endpoint; only what is
// worth alerting on becomes a time series.
//
// Every label used here is bounded: routes are labelled by their gateway path
// template and operation id, never by the concrete request path, so a client
// cannot blow up cardinality by varying an id.
type Metrics struct {
	Requests         *prometheus.CounterVec
	UpstreamDuration *prometheus.HistogramVec
	UpstreamErrors   *prometheus.CounterVec
	HostsHealthy     *prometheus.GaugeVec

	AuthTotal      *prometheus.CounterVec
	RateLimitTotal *prometheus.CounterVec

	BuildInfo *prometheus.GaugeVec
}

// New registers the metric set with r.
func New(r prometheus.Registerer, version string) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_requests_total",
			Help: "Requests handled, by route and response status class.",
		}, []string{"route", "method", "status"}),

		UpstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nina_upstream_duration_seconds",
			Help:    "Time spent waiting on the upstream.",
			Buckets: prometheus.DefBuckets,
		}, []string{"backend"}),

		UpstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_upstream_errors_total",
			Help: "Upstream failures by kind.",
		}, []string{"backend", "kind"}),

		HostsHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nina_upstream_hosts_healthy",
			Help: "Upstream hosts currently passing their health check, by backend.",
		}, []string{"backend"}),

		AuthTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_auth_total",
			Help: "Authentication outcomes.",
		}, []string{"middleware", "result"}),

		RateLimitTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nina_ratelimit_total",
			Help: "Rate limit decisions.",
		}, []string{"middleware", "decision"}),

		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nina_build_info",
			Help: "Build information; always 1.",
		}, []string{"version"}),
	}

	r.MustRegister(
		m.Requests, m.UpstreamDuration, m.UpstreamErrors, m.HostsHealthy,
		m.AuthTotal, m.RateLimitTotal, m.BuildInfo,
	)
	m.BuildInfo.WithLabelValues(version).Set(1)
	return m
}

// StatusClass buckets a response code to its class. The exact code is on the
// access log line.
func StatusClass(code int) string {
	if code < 100 || code > 599 {
		return "unknown"
	}
	return strconv.Itoa(code/100) + "xx"
}

// The following satisfy chain.Metrics, which keeps internal/mw free of a
// Prometheus dependency.

func (m *Metrics) CountAuth(middleware, result string) {
	m.AuthTotal.WithLabelValues(middleware, result).Inc()
}

func (m *Metrics) CountRateLimit(middleware, decision string) {
	m.RateLimitTotal.WithLabelValues(middleware, decision).Inc()
}
