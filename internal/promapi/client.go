// Package promapi retrieves historical container resource usage from
// Prometheus.
//
// This package is the production counterpart of internal/simulator: both
// produce model.Series, and the recommendation engine cannot tell which one it
// is reading. That symmetry is what lets experimental findings apply to the
// deployed system.
package promapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/anishc23/k8s-cost-optimizer/internal/metrics"
	dm "github.com/anishc23/k8s-cost-optimizer/internal/model"
)

// Source retrieves usage series for containers.
//
// It is an interface so that the API server, the controller and the tests can
// be wired to a fake without a Prometheus instance, and so that an alternative
// backend (a metrics store other than Prometheus) can be substituted without
// touching the recommendation engine.
type Source interface {
	// CPUUsage returns a CPU usage series in millicores.
	CPUUsage(ctx context.Context, q Query) (dm.Series, error)
	// MemoryUsage returns a working-set series in bytes.
	MemoryUsage(ctx context.Context, q Query) (dm.Series, error)
	// Throttling returns the CFS throttled-period ratio over the window, and
	// whether the metric was available at all. Availability is reported
	// separately because an unavailable metric must not be read as "no
	// throttling".
	Throttling(ctx context.Context, q Query) (ratio float64, available bool, err error)
	// Healthy reports whether the backend is reachable.
	Healthy(ctx context.Context) error
}

// Query identifies the series to retrieve.
type Query struct {
	Namespace string
	// Pod is a regex matching the pods of one workload. Queries are issued per
	// workload rather than per pod so that the engine sees the aggregate
	// behaviour of the replica set, which is what a single shared request must
	// accommodate.
	PodRegex  string
	Container string
	Window    time.Duration
	Step      time.Duration
	End       time.Time
}

// Config configures the client.
type Config struct {
	Address string
	Timeout time.Duration
	// Step is the query resolution. It trades fidelity against query cost:
	// shorter steps reveal brief bursts but multiply the samples Prometheus must
	// return, and a step below the scrape interval yields interpolated points
	// that carry no additional information.
	Step time.Duration
	// BearerTokenFile, when set, authenticates to a protected Prometheus.
	BearerTokenFile string
	// InsecureSkipVerify disables TLS verification. Defaults off and should stay
	// off outside development clusters.
	InsecureSkipVerify bool
}

// Client is the Prometheus-backed Source.
type Client struct {
	api     promv1.API
	cfg     Config
	log     *slog.Logger
	metrics *metrics.Metrics
}

// NewClient constructs a Prometheus client.
func NewClient(cfg Config, log *slog.Logger, m *metrics.Metrics) (*Client, error) {
	if cfg.Address == "" {
		return nil, errors.New("prometheus address is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Step <= 0 {
		cfg.Step = time.Minute
	}
	rt := promapi.DefaultRoundTripper
	if cfg.BearerTokenFile != "" {
		var err error
		rt, err = newBearerTokenRoundTripper(cfg.BearerTokenFile, rt)
		if err != nil {
			return nil, err
		}
	}
	c, err := promapi.NewClient(promapi.Config{
		Address:      cfg.Address,
		RoundTripper: rt,
	})
	if err != nil {
		return nil, fmt.Errorf("create prometheus client: %w", err)
	}
	return &Client{api: promv1.NewAPI(c), cfg: cfg, log: log, metrics: m}, nil
}

// CPU usage query.
//
// rate() over container_cpu_usage_seconds_total is used rather than irate()
// because irate() reports the instantaneous rate between the final two samples
// in each step, which for a right-sizing decision over days is a needlessly
// noisy estimator: it amplifies scrape jitter into apparent spikes. rate()
// averages across the window and is the correct choice when the question is
// "how much CPU does this container consume", not "what did it do in the last
// two scrapes".
//
// The consequence, which is stated in research/limitations.md, is that bursts
// shorter than the rate window are smoothed away. The rate window is set to
// four times the step so that every point is computed from several scrapes
// (Prometheus requires at least two samples in the window) while remaining
// short enough to retain sub-hour structure.
//
// The empty-container-name exclusion removes the pod-level cgroup summary
// series, which would otherwise double-count every container's usage.
const cpuQueryTemplate = `sum(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s",container="%s",container!="",container!="POD"}[%s])) * 1000`

// Memory usage query.
//
// container_memory_working_set_bytes is used rather than
// container_memory_usage_bytes because the working set excludes reclaimable
// page cache, and it is the working set that the kernel OOM killer compares
// against the limit. Sizing against usage_bytes would systematically
// over-provision every container that reads files, since its page cache would be
// counted as demand that must be reserved.
const memoryQueryTemplate = `sum(container_memory_working_set_bytes{namespace="%s",pod=~"%s",container="%s",container!="",container!="POD"})`

// Throttling ratio: throttled periods over elapsed periods.
const throttleQueryTemplate = `sum(rate(container_cpu_cfs_throttled_periods_total{namespace="%s",pod=~"%s",container="%s"}[%s])) / clamp_min(sum(rate(container_cpu_cfs_periods_total{namespace="%s",pod=~"%s",container="%s"}[%s])), 1)`

// CPUUsage retrieves the CPU usage series in millicores.
func (c *Client) CPUUsage(ctx context.Context, q Query) (dm.Series, error) {
	step := c.step(q)
	rateWindow := 4 * step
	query := fmt.Sprintf(cpuQueryTemplate, q.Namespace, q.PodRegex, q.Container, promDuration(rateWindow))
	samples, err := c.rangeQuery(ctx, "cpu", query, q, step)
	if err != nil {
		return dm.Series{}, err
	}
	return dm.Series{Resource: dm.ResourceCPU, Samples: samples, Step: step}, nil
}

// MemoryUsage retrieves the working-set series in bytes.
func (c *Client) MemoryUsage(ctx context.Context, q Query) (dm.Series, error) {
	step := c.step(q)
	query := fmt.Sprintf(memoryQueryTemplate, q.Namespace, q.PodRegex, q.Container)
	samples, err := c.rangeQuery(ctx, "memory", query, q, step)
	if err != nil {
		return dm.Series{}, err
	}
	return dm.Series{Resource: dm.ResourceMemory, Samples: samples, Step: step}, nil
}

// Throttling returns the mean CFS throttled-period ratio over the window.
//
// An empty result means the metric is not exposed by this cAdvisor build or the
// container has no CPU limit (and so no CFS quota). That is reported as
// "unavailable" rather than zero: treating a missing metric as evidence of no
// throttling would let the engine act confidently on information it never had.
func (c *Client) Throttling(ctx context.Context, q Query) (float64, bool, error) {
	step := c.step(q)
	rateWindow := 4 * step
	query := fmt.Sprintf(throttleQueryTemplate,
		q.Namespace, q.PodRegex, q.Container, promDuration(rateWindow),
		q.Namespace, q.PodRegex, q.Container, promDuration(rateWindow))
	samples, err := c.rangeQuery(ctx, "throttling", query, q, step)
	if err != nil {
		return 0, false, err
	}
	if len(samples) == 0 {
		return 0, false, nil
	}
	var sum float64
	for _, s := range samples {
		sum += s.Value
	}
	return sum / float64(len(samples)), true, nil
}

// Healthy checks that the backend answers a trivial query.
func (c *Client) Healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	_, warnings, err := c.api.Query(ctx, "vector(1)", time.Now())
	if err != nil {
		return fmt.Errorf("prometheus health check: %w", err)
	}
	if len(warnings) > 0 {
		c.log.Warn("prometheus health check returned warnings", "warnings", warnings)
	}
	return nil
}

func (c *Client) step(q Query) time.Duration {
	if q.Step > 0 {
		return q.Step
	}
	return c.cfg.Step
}

// rangeQuery executes a range query and converts the result to samples.
func (c *Client) rangeQuery(ctx context.Context, kind, query string, q Query, step time.Duration) ([]dm.Sample, error) {
	end := q.End
	if end.IsZero() {
		end = time.Now()
	}
	r := promv1.Range{Start: end.Add(-q.Window), End: end, Step: step}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	start := time.Now()
	value, warnings, err := c.api.QueryRange(ctx, query, r)
	elapsed := time.Since(start)
	if c.metrics != nil {
		c.metrics.PromQueryDuration.WithLabelValues(kind).Observe(elapsed.Seconds())
	}
	if err != nil {
		if c.metrics != nil {
			c.metrics.PromQueryErrors.WithLabelValues(kind, classifyError(err)).Inc()
		}
		return nil, fmt.Errorf("prometheus range query (%s): %w", kind, err)
	}
	if len(warnings) > 0 {
		// Warnings usually mean the query hit a limit and the result is
		// truncated, which would silently bias the statistics. They are logged
		// rather than ignored so the cause of an odd recommendation is findable.
		c.log.Warn("prometheus query returned warnings",
			"kind", kind, "namespace", q.Namespace, "container", q.Container, "warnings", warnings)
	}

	matrix, ok := value.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("prometheus range query (%s): expected a matrix, got %T", kind, value)
	}
	if len(matrix) == 0 {
		// No series is not an error: a workload may genuinely have no metrics
		// (just deployed, or its exporter is down). The engine's sufficiency gate
		// is responsible for deciding what to do about that, and it needs to see
		// an empty series rather than a failure.
		return nil, nil
	}
	if len(matrix) > 1 {
		// The queries aggregate with sum(), so more than one series means the
		// label matchers did not disambiguate. Returning the first would silently
		// analyse an arbitrary subset of the workload.
		return nil, fmt.Errorf(
			"prometheus range query (%s) for %s/%s returned %d series; the label selectors do not identify a single workload",
			kind, q.Namespace, q.Container, len(matrix))
	}

	pairs := matrix[0].Values
	samples := make([]dm.Sample, 0, len(pairs))
	for _, p := range pairs {
		v := float64(p.Value)
		// NaN appears where a division had no denominator (the throttling query
		// on a container with no CFS periods). Dropping the sample is correct:
		// it carries no measurement.
		if isNaN(v) {
			continue
		}
		samples = append(samples, dm.Sample{Timestamp: p.Timestamp.Time(), Value: v})
	}
	if c.metrics != nil {
		c.metrics.PromSamplesReturned.Observe(float64(len(samples)))
	}
	return samples, nil
}

func isNaN(f float64) bool { return f != f }

// classifyError buckets errors for the error counter. The labels are kept
// low-cardinality on purpose: a label per error string would blow up the metric.
func classifyError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "connection refused"), strings.Contains(s, "no such host"):
		return "unreachable"
	case strings.Contains(s, "401"), strings.Contains(s, "403"):
		return "unauthorized"
	case strings.Contains(s, "429"):
		return "rate_limited"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"):
		return "timeout"
	default:
		return "other"
	}
}

// promDuration renders a Go duration in Prometheus duration syntax.
//
// Go's Duration.String() produces forms like "1m0s" and "1h0m0s" that
// Prometheus rejects, so the conversion is explicit rather than a String() call.
func promDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		if int(d.Seconds())%60 == 0 {
			return fmt.Sprintf("%dm", int(d.Minutes()))
		}
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if int(d.Minutes())%60 == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// bearerTokenRoundTripper attaches a token read fresh from disk on each request.
//
// Reading per request rather than once at startup matters in Kubernetes:
// projected service account tokens are rotated by the kubelet, and a token
// cached at startup stops working partway through the pod's life. The failure
// would appear as unauthorized errors hours after a successful start, which is
// an unpleasant thing to debug.
type bearerTokenRoundTripper struct {
	path string
	next http.RoundTripper
}

func newBearerTokenRoundTripper(path string, next http.RoundTripper) (http.RoundTripper, error) {
	rt := &bearerTokenRoundTripper{path: path, next: next}
	if _, err := rt.token(); err != nil {
		return nil, fmt.Errorf("read bearer token file: %w", err)
	}
	return rt, nil
}

func (rt *bearerTokenRoundTripper) token() (string, error) {
	b, err := readFile(rt.path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (rt *bearerTokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := rt.token()
	if err != nil {
		return nil, fmt.Errorf("read bearer token: %w", err)
	}
	// The request must be cloned: RoundTrippers may not modify the request they
	// are given, and mutating a retried request would append duplicate headers.
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return rt.next.RoundTrip(r)
}
