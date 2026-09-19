// Package metrics defines the Prometheus instrumentation for the optimizer
// itself.
//
// A tool that advises on resource usage should be observable on the same terms
// it holds others to. Beyond that principle, these metrics exist to answer
// specific operational questions that would otherwise require reading logs:
// whether recommendations are being generated at all, whether they are being
// suppressed by safety gates, and whether the Prometheus backend is healthy
// enough for the recommendations to mean anything.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds every collector the optimizer exposes.
type Metrics struct {
	Registry *prometheus.Registry

	WorkloadsDiscovered      prometheus.Gauge
	RecommendationsGenerated *prometheus.CounterVec
	AnalysisDuration         prometheus.Histogram
	AnalysisErrors           prometheus.Counter
	LastAnalysisTimestamp    prometheus.Gauge

	PromQueryDuration   *prometheus.HistogramVec
	PromQueryErrors     *prometheus.CounterVec
	PromSamplesReturned prometheus.Histogram

	KubeAPIErrors *prometheus.CounterVec

	// GatesFired is the metric that makes the safety machinery visible. A spike
	// in oom-protection firings means a set of workloads started failing; a
	// spike in data-sufficiency firings usually means the metrics pipeline broke,
	// not that the workloads changed.
	GatesFired *prometheus.CounterVec

	EstimatedMonthlyCost    prometheus.Gauge
	EstimatedMonthlySavings prometheus.Gauge

	APIRequests *prometheus.CounterVec
	APIDuration *prometheus.HistogramVec
}

// New builds and registers the collectors on a fresh registry.
//
// A dedicated registry rather than the default one keeps the optimizer's own
// metrics separate from any library that registers globally, and makes the
// collector set testable.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	f := promauto.With(reg)

	return &Metrics{
		Registry: reg,
		WorkloadsDiscovered: f.NewGauge(prometheus.GaugeOpts{
			Name: "optimizer_workloads_discovered",
			Help: "Number of workloads discovered in the most recent analysis cycle.",
		}),
		RecommendationsGenerated: f.NewCounterVec(prometheus.CounterOpts{
			Name: "optimizer_recommendations_generated_total",
			Help: "Recommendations generated, partitioned by resource and decision.",
		}, []string{"resource", "decision"}),
		AnalysisDuration: f.NewHistogram(prometheus.HistogramOpts{
			Name: "optimizer_analysis_duration_seconds",
			Help: "Wall-clock duration of a full analysis cycle.",
			// Buckets span 100ms to ~13 minutes: a cycle over a few workloads
			// completes in well under a second, while a large cluster is
			// dominated by Prometheus round trips and can take minutes.
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 13),
		}),
		AnalysisErrors: f.NewCounter(prometheus.CounterOpts{
			Name: "optimizer_analysis_errors_total",
			Help: "Analysis cycles that failed.",
		}),
		LastAnalysisTimestamp: f.NewGauge(prometheus.GaugeOpts{
			Name: "optimizer_last_analysis_timestamp_seconds",
			Help: "Unix timestamp of the last successful analysis. Alert on staleness: recommendations served after a long gap describe a cluster that no longer exists.",
		}),
		PromQueryDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "optimizer_prometheus_query_duration_seconds",
			Help:    "Duration of Prometheus range queries, by query kind.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
		}, []string{"query"}),
		PromQueryErrors: f.NewCounterVec(prometheus.CounterOpts{
			Name: "optimizer_prometheus_query_errors_total",
			Help: "Failed Prometheus queries, by query kind and error class.",
		}, []string{"query", "reason"}),
		PromSamplesReturned: f.NewHistogram(prometheus.HistogramOpts{
			Name:    "optimizer_prometheus_samples_returned",
			Help:    "Samples returned per series. A collapse toward zero indicates a metrics pipeline problem, which would otherwise surface only as a wave of INSUFFICIENT_DATA decisions.",
			Buckets: prometheus.ExponentialBuckets(1, 4, 10),
		}),
		KubeAPIErrors: f.NewCounterVec(prometheus.CounterOpts{
			Name: "optimizer_kubernetes_api_errors_total",
			Help: "Kubernetes API errors, by operation.",
		}, []string{"operation"}),
		GatesFired: f.NewCounterVec(prometheus.CounterOpts{
			Name: "optimizer_gates_fired_total",
			Help: "Safety gates that modified or blocked a recommendation, by gate and resource.",
		}, []string{"gate", "resource"}),
		EstimatedMonthlyCost: f.NewGauge(prometheus.GaugeOpts{
			Name: "optimizer_estimated_monthly_cost_usd",
			Help: "Allocation-based monthly cost of current requests across analysed workloads.",
		}),
		EstimatedMonthlySavings: f.NewGauge(prometheus.GaugeOpts{
			Name: "optimizer_estimated_monthly_savings_usd",
			Help: "Allocation-based monthly saving if all recommendations were applied. This is an upper bound: see docs/cost-model.md.",
		}),
		APIRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "optimizer_api_requests_total",
			Help: "HTTP API requests, by route and status class.",
		}, []string{"route", "status"}),
		APIDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "optimizer_api_request_duration_seconds",
			Help:    "HTTP API request duration by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
	}
}
