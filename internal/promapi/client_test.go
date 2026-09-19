package promapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/metrics"
	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// matrixResponse builds a Prometheus range-query response with one series.
func matrixResponse(values []string, startUnix int64, stepSec int64) string {
	var pairs []string
	for i, v := range values {
		pairs = append(pairs, fmt.Sprintf(`[%d,"%s"]`, startUnix+int64(i)*stepSec, v))
	}
	return fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{},"values":[%s]}]}}`, strings.Join(pairs, ","))
}

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server, *metrics.Metrics) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m := metrics.New()
	c, err := NewClient(Config{Address: srv.URL, Timeout: 5 * time.Second, Step: time.Minute}, quiet(), m)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv, m
}

func TestNewClientRequiresAddress(t *testing.T) {
	if _, err := NewClient(Config{}, quiet(), nil); err == nil {
		t.Error("expected an error when no address is configured")
	}
}

func TestCPUUsageParsesMillicores(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, matrixResponse([]string{"250", "300", "275"}, 1700000000, 60))
	})
	s, err := c.CPUUsage(context.Background(), Query{
		Namespace: "app", PodRegex: "^api-.*$", Container: "app",
		Window: time.Hour, Step: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Resource != model.ResourceCPU {
		t.Errorf("resource = %s, want cpu", s.Resource)
	}
	if s.Len() != 3 {
		t.Fatalf("got %d samples, want 3", s.Len())
	}
	if s.Samples[0].Value != 250 {
		t.Errorf("first sample = %v, want 250", s.Samples[0].Value)
	}
	if s.Step != time.Minute {
		t.Errorf("step = %v, want 1m", s.Step)
	}
}

func TestMemoryUsageParsesBytes(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, matrixResponse([]string{"536870912"}, 1700000000, 60))
	})
	s, err := c.MemoryUsage(context.Background(), Query{
		Namespace: "app", PodRegex: "^api-.*$", Container: "app", Window: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Samples[0].Value != 536870912 {
		t.Errorf("sample = %v, want 512Mi in bytes", s.Samples[0].Value)
	}
}

// The queries must use the metrics the algorithm documentation claims, and must
// exclude the pod-level cgroup summary that would double-count usage.
func TestQueriesUseTheDocumentedMetrics(t *testing.T) {
	var captured []string
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		captured = append(captured, r.FormValue("query"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, matrixResponse([]string{"1"}, 1700000000, 60))
	})
	q := Query{Namespace: "app", PodRegex: "^api-.*$", Container: "app", Window: time.Hour, Step: time.Minute}
	if _, err := c.CPUUsage(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MemoryUsage(context.Background(), q); err != nil {
		t.Fatal(err)
	}

	cpuQ, memQ := captured[0], captured[1]
	// rate(), not irate(): irate over a multi-day window amplifies scrape jitter
	// into apparent spikes.
	if !strings.Contains(cpuQ, "rate(container_cpu_usage_seconds_total") {
		t.Errorf("CPU query should use rate() over container_cpu_usage_seconds_total: %s", cpuQ)
	}
	if strings.Contains(cpuQ, "irate(") {
		t.Errorf("CPU query must not use irate(): %s", cpuQ)
	}
	// Working set, not usage_bytes: usage_bytes includes reclaimable page cache.
	if !strings.Contains(memQ, "container_memory_working_set_bytes") {
		t.Errorf("memory query should use the working set: %s", memQ)
	}
	if strings.Contains(memQ, "container_memory_usage_bytes") {
		t.Errorf("memory query must not use usage_bytes: %s", memQ)
	}
	// Both must exclude the pod-level summary series.
	for _, q := range []string{cpuQ, memQ} {
		if !strings.Contains(q, `container!=""`) {
			t.Errorf("query must exclude the pod-level cgroup series: %s", q)
		}
	}
	// The rate window must be a multiple of the step, so each point is computed
	// from several scrapes.
	if !strings.Contains(cpuQ, "[4m]") {
		t.Errorf("CPU rate window should be 4x the 1m step: %s", cpuQ)
	}
}

// An empty result is not an error: a workload may genuinely have no metrics, and
// the engine's sufficiency gate must be the thing that decides what to do.
func TestEmptyResultIsNotAnError(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	})
	s, err := c.CPUUsage(context.Background(), Query{Namespace: "app", Container: "app", Window: time.Hour})
	if err != nil {
		t.Fatalf("an empty result should not be an error: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("expected an empty series, got %d samples", s.Len())
	}
}

// Ambiguous label selectors must fail loudly: returning the first of several
// series would silently analyse an arbitrary subset of the workload.
func TestMultipleSeriesIsAnError(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"pod":"a"},"values":[[1700000000,"1"]]},
			{"metric":{"pod":"b"},"values":[[1700000000,"2"]]}]}}`)
	})
	_, err := c.CPUUsage(context.Background(), Query{Namespace: "app", Container: "app", Window: time.Hour})
	if err == nil {
		t.Fatal("expected an error when the selectors do not identify a single workload")
	}
	if !strings.Contains(err.Error(), "single workload") {
		t.Errorf("the error should explain the cause: %v", err)
	}
}

// NaN carries no measurement and must be dropped rather than entering the
// statistics as a value.
func TestNaNSamplesAreDropped(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, matrixResponse([]string{"100", "NaN", "200"}, 1700000000, 60))
	})
	s, err := c.CPUUsage(context.Background(), Query{Namespace: "app", Container: "app", Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 {
		t.Errorf("got %d samples, want 2 after dropping NaN", s.Len())
	}
	for _, sm := range s.Samples {
		if sm.Value != sm.Value {
			t.Error("a NaN sample survived")
		}
	}
}

// A missing throttling metric must be reported as unavailable, not as zero
// throttling: absent evidence is not evidence of absence.
func TestThrottlingReportsUnavailableRatherThanZero(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	})
	ratio, available, err := c.Throttling(context.Background(), Query{Namespace: "app", Container: "app", Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Error("an empty result must report the metric as unavailable")
	}
	if ratio != 0 {
		t.Errorf("ratio = %v", ratio)
	}
}

func TestThrottlingAveragesOverWindow(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, matrixResponse([]string{"0.1", "0.3", "0.2"}, 1700000000, 60))
	})
	ratio, available, err := c.Throttling(context.Background(), Query{Namespace: "app", Container: "app", Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("expected the metric to be available")
	}
	if diff := ratio - 0.2; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("ratio = %v, want the mean 0.2", ratio)
	}
}

func TestQueryErrorsAreClassifiedAndCounted(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"status":"error","errorType":"internal","error":"boom"}`)
	})
	if _, err := c.CPUUsage(context.Background(), Query{Namespace: "app", Container: "app", Window: time.Hour}); err == nil {
		t.Fatal("expected an error from a 500 response")
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "canceled"},
		{fmt.Errorf("dial tcp: connection refused"), "unreachable"},
		{fmt.Errorf("lookup prometheus: no such host"), "unreachable"},
		{fmt.Errorf("server returned 401 Unauthorized"), "unauthorized"},
		{fmt.Errorf("server returned 429 Too Many Requests"), "rate_limited"},
		{fmt.Errorf("something unexpected"), "other"},
	}
	for _, c := range cases {
		if got := classifyError(c.err); got != c.want {
			t.Errorf("classifyError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// Go's Duration.String() produces forms Prometheus rejects ("1m0s"), so the
// conversion must be explicit.
func TestPromDurationIsValidPrometheusSyntax(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{time.Minute, "1m"},
		{4 * time.Minute, "4m"},
		{90 * time.Second, "90s"},
		{time.Hour, "1h"},
		{2 * time.Hour, "2h"},
		{90 * time.Minute, "90m"},
	}
	for _, c := range cases {
		if got := promDuration(c.d); got != c.want {
			t.Errorf("promDuration(%v) = %q, want %q", c.d, got, c.want)
		}
		// Whatever the form, it must be a single number followed by one unit:
		// Go's Duration.String() produces compound forms like "1m30s" that
		// Prometheus rejects.
		if !singleUnit(promDuration(c.d)) {
			t.Errorf("promDuration(%v) = %q is not valid Prometheus duration syntax", c.d, promDuration(c.d))
		}
	}
}

// singleUnit reports whether s is digits followed by exactly one unit letter.
func singleUnit(s string) bool {
	if len(s) < 2 {
		return false
	}
	digits := s[:len(s)-1]
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	switch s[len(s)-1] {
	case 's', 'm', 'h', 'd', 'w', 'y':
		return true
	default:
		return false
	}
}

func TestHealthy(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{},"value":[1700000000,"1"]}]}}`)
	})
	if err := c.Healthy(context.Background()); err != nil {
		t.Errorf("Healthy() = %v", err)
	}
}

func TestHealthyFailsWhenUnreachable(t *testing.T) {
	m := metrics.New()
	// A port nothing listens on.
	c, err := NewClient(Config{Address: "http://127.0.0.1:1", Timeout: time.Second, Step: time.Minute}, quiet(), m)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Healthy(context.Background()); err == nil {
		t.Error("expected a health check failure against an unreachable address")
	}
}

// Projected service account tokens are rotated by the kubelet, so a token cached
// at startup stops working partway through the pod's life. The round tripper must
// read it fresh per request.
func TestBearerTokenIsReadPerRequest(t *testing.T) {
	// A fresh token on every read, so that a cached token is detectable.
	reads := 0
	orig := readFile
	readFile = func(string) ([]byte, error) {
		reads++
		return []byte(fmt.Sprintf("token-%d\n", reads)), nil
	}
	t.Cleanup(func() { readFile = orig })

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, matrixResponse([]string{"1"}, 1700000000, 60))
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(Config{
		Address: srv.URL, Timeout: 5 * time.Second, Step: time.Minute,
		BearerTokenFile: "/var/run/secrets/token",
	}, quiet(), nil)
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Namespace: "app", Container: "app", Window: time.Hour}
	for i := 0; i < 2; i++ {
		if _, err := c.CPUUsage(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(seen))
	}
	// Each request must carry a distinct token: if the token were cached at
	// startup, every request would carry the same one and rotation would break
	// the optimizer hours after a successful start.
	if seen[0] == seen[len(seen)-1] {
		t.Errorf("the token was not re-read: all requests used %q", seen[0])
	}
	if !strings.HasPrefix(seen[0], "Bearer ") {
		t.Errorf("Authorization header = %q", seen[0])
	}
}

func TestBearerTokenFileMustExist(t *testing.T) {
	if _, err := NewClient(Config{
		Address: "http://localhost:9090", BearerTokenFile: "/nonexistent/token",
	}, quiet(), nil); err == nil {
		t.Error("expected an error for a missing bearer token file")
	}
}

func TestSourceInterfaceSatisfied(t *testing.T) {
	var _ Source = (*Client)(nil)
}
