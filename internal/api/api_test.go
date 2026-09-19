package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/anishc23/k8s-cost-optimizer/internal/cost"
	"github.com/anishc23/k8s-cost-optimizer/internal/kube"
	"github.com/anishc23/k8s-cost-optimizer/internal/metrics"
	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/internal/promapi"
	"github.com/anishc23/k8s-cost-optimizer/internal/recommender"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeSource is a deterministic promapi.Source. It returns constant series so
// that a test asserts on the engine's behaviour rather than on generated noise.
type fakeSource struct {
	cpuMilli   float64
	memBytes   float64
	samples    int
	step       time.Duration
	throttle   float64
	available  bool
	failCPU    bool
	failMemory bool
}

func newFakeSource(cpuMilli, memBytes float64) *fakeSource {
	return &fakeSource{cpuMilli: cpuMilli, memBytes: memBytes, samples: 500, step: time.Minute, available: true}
}

func (f *fakeSource) series(kind model.ResourceKind, v float64) model.Series {
	end := time.Now()
	out := make([]model.Sample, f.samples)
	for i := range out {
		out[i] = model.Sample{
			Timestamp: end.Add(-time.Duration(f.samples-1-i) * f.step),
			Value:     v,
		}
	}
	return model.Series{Resource: kind, Samples: out, Step: f.step}
}

func (f *fakeSource) CPUUsage(ctx context.Context, q promapi.Query) (model.Series, error) {
	if f.failCPU {
		return model.Series{}, errors.New("simulated prometheus failure")
	}
	return f.series(model.ResourceCPU, f.cpuMilli), nil
}

func (f *fakeSource) MemoryUsage(ctx context.Context, q promapi.Query) (model.Series, error) {
	if f.failMemory {
		return model.Series{}, errors.New("simulated prometheus failure")
	}
	return f.series(model.ResourceMemory, f.memBytes), nil
}

func (f *fakeSource) Throttling(ctx context.Context, q promapi.Query) (float64, bool, error) {
	return f.throttle, f.available, nil
}

func (f *fakeSource) Healthy(ctx context.Context) error { return nil }

func deployment(ns, name string, replicas int32, cpu, mem string) *appsv1.Deployment {
	mk := func(s string) resource.Quantity {
		q, err := resource.ParseQuantity(s)
		if err != nil {
			panic(err)
		}
		return q
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU:    mk(cpu),
						corev1.ResourceMemory: mk(mem),
					}},
				}},
			}},
		},
	}
}

// oomPod returns a pod whose container was previously OOMKilled, so that the
// engine's memory gate has real evidence to react to.
func oomPod(ns, deploymentName string) *corev1.Pod {
	finished := metav1.NewTime(time.Now().Add(-time.Hour))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: deploymentName + "-7d4b8c9f5d-x2k9p",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: deploymentName + "-7d4b8c9f5d"}},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", RestartCount: 3,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "OOMKilled", ExitCode: 137, FinishedAt: finished,
			}},
		}}},
	}
}

// healthyPod returns a pod with clean evidence, which the engine requires before
// it will reduce memory at all.
func healthyPod(ns, deploymentName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: deploymentName + "-7d4b8c9f5d-x2k9p",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: deploymentName + "-7d4b8c9f5d"}},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", RestartCount: 0,
		}}},
	}
}

func newTestService(t *testing.T, objs []runtimeObject, src promapi.Source, policy recommender.Policy) *Service {
	t.Helper()
	svc, _ := newTestServiceWithMetrics(t, objs, src, policy)
	return svc
}

// newTestServiceWithMetrics also returns the registry, so that a test asserting
// on exported metrics observes the same registry the service writes to.
func newTestServiceWithMetrics(t *testing.T, objs []runtimeObject, src promapi.Source, policy recommender.Policy) (*Service, *metrics.Metrics) {
	t.Helper()
	cs := fake.NewSimpleClientset(toRuntime(objs)...)
	m := metrics.New()
	disc := kube.NewClientWithInterface(cs, kube.DefaultOptions(), quiet(), m)
	eng, err := recommender.NewEngine(policy)
	if err != nil {
		t.Fatal(err)
	}
	est, err := cost.NewEstimator(cost.DefaultModel())
	if err != nil {
		t.Fatal(err)
	}
	return NewService(disc, src, eng, est, Options{Concurrency: 4, QueryStep: time.Minute}, quiet(), m), m
}

func testPolicy() recommender.Policy {
	p := recommender.DefaultPolicy()
	// The fake source produces 500 one-minute samples (~8h), so the window and
	// thresholds are set to accept that rather than rejecting it as too short.
	p.ObservationWindow = 8 * time.Hour
	p.MinSamples = 60
	p.MinDuration = time.Hour
	return p
}

// --- analysis -------------------------------------------------------------

func TestAnalyzeProducesRecommendationsAndCost(t *testing.T) {
	svc := newTestService(t,
		[]runtimeObject{
			ns("app"),
			deployment("app", "api", 3, "2", "4Gi"),
			healthyPod("app", "api"),
		},
		newFakeSource(200, 500*float64(model.BytesPerMi)),
		testPolicy())

	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := svc.Snapshot()
	if !snap.Ready {
		t.Fatal("snapshot should be ready after a successful cycle")
	}
	if len(snap.Recommendations) != 1 {
		t.Fatalf("got %d recommendations, want 1", len(snap.Recommendations))
	}
	rec := snap.Recommendations[0]
	c := rec.Containers[0]
	if c.CPU.Decision != model.DecisionDecrease {
		t.Errorf("CPU decision = %s, want DECREASE (reason: %s)", c.CPU.Decision, c.CPU.Reason)
	}
	if c.Memory.Decision != model.DecisionDecrease {
		t.Errorf("memory decision = %s, want DECREASE (reason: %s)", c.Memory.Decision, c.Memory.Reason)
	}
	if rec.Cost.MonthlySavingsUSD <= 0 {
		t.Errorf("expected a positive saving, got %.2f", rec.Cost.MonthlySavingsUSD)
	}
	if snap.Summary.Workloads != 1 || snap.Summary.Decreases != 2 {
		t.Errorf("unexpected summary: %+v", snap.Summary)
	}
}

// OOM evidence discovered from the cluster must reach the engine and block the
// memory reduction. This is the end-to-end path of the project's central safety
// property.
func TestOOMEvidenceFromClusterBlocksMemoryReduction(t *testing.T) {
	svc := newTestService(t,
		[]runtimeObject{
			ns("app"),
			deployment("app", "api", 1, "2", "4Gi"),
			oomPod("app", "api"),
		},
		newFakeSource(200, 500*float64(model.BytesPerMi)),
		testPolicy())

	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := svc.Snapshot().Recommendations[0].Containers[0]
	if c.Memory.Decision != model.DecisionBlocked {
		t.Errorf("memory decision = %s, want BLOCKED (reason: %s)", c.Memory.Decision, c.Memory.Reason)
	}
	if c.CPU.Decision != model.DecisionDecrease {
		t.Errorf("CPU decision = %s: an OOM is not evidence about CPU", c.CPU.Decision)
	}
	// The blocked memory must contribute no saving.
	wantSaving := cost.DefaultModel().MonthlyUSD(model.Millicores(2000-c.CPU.Target), 0)
	got := svc.Snapshot().Recommendations[0].Cost.MonthlySavingsUSD
	if diff := got - wantSaving; diff > 0.01 || diff < -0.01 {
		t.Errorf("saving %.4f should come from CPU alone (%.4f)", got, wantSaving)
	}
}

// A workload that cannot be analysed must be reported, not silently dropped: an
// invisible omission would understate cluster cost and hide a broken pipeline.
func TestFailedWorkloadIsReportedNotDropped(t *testing.T) {
	src := newFakeSource(200, 500e6)
	src.failCPU = true
	svc := newTestService(t,
		[]runtimeObject{ns("app"), deployment("app", "api", 1, "2", "4Gi")},
		src, testPolicy())

	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatalf("a per-workload failure must not fail the cycle: %v", err)
	}
	snap := svc.Snapshot()
	if len(snap.Errors) != 1 {
		t.Fatalf("expected 1 reported error, got %d", len(snap.Errors))
	}
	if snap.Errors[0].Workload != "app/api" {
		t.Errorf("error attributed to %q", snap.Errors[0].Workload)
	}
	if len(snap.Recommendations) != 0 {
		t.Error("a failed workload must not appear as a recommendation")
	}
	if snap.Summary.CurrentMonthlyUSD != 0 {
		t.Error("a failed workload must not contribute to the cost summary")
	}
}

// Throttling evidence is supplementary: its absence must not fail the workload.
func TestUnavailableThrottlingDoesNotFailAnalysis(t *testing.T) {
	src := newFakeSource(200, 500e6)
	src.available = false
	svc := newTestService(t,
		[]runtimeObject{ns("app"), deployment("app", "api", 1, "2", "4Gi"), healthyPod("app", "api")},
		src, testPolicy())
	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(svc.Snapshot().Recommendations) != 1 {
		t.Error("analysis should succeed without throttling metrics")
	}
}

func TestSnapshotIsConsistentUnderConcurrentReads(t *testing.T) {
	svc := newTestService(t,
		[]runtimeObject{ns("app"), deployment("app", "api", 1, "2", "4Gi"), healthyPod("app", "api")},
		newFakeSource(200, 500e6), testPolicy())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			snap := svc.Snapshot()
			// The summary must always describe exactly the recommendations in the
			// same snapshot; a torn read would break this.
			if snap.Ready && snap.Summary.Workloads != len(snap.Recommendations) {
				t.Errorf("torn snapshot: summary reports %d workloads but %d recommendations are present",
					snap.Summary.Workloads, len(snap.Recommendations))
				return
			}
		}
	}()
	for i := 0; i < 10; i++ {
		if err := svc.Analyze(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}

// --- HTTP -----------------------------------------------------------------

func newTestServer(t *testing.T, svc *Service) *Server {
	t.Helper()
	return NewServer(svc, quiet(), metrics.New(),
		[]NamedCheck{{Name: "prometheus", Check: func(context.Context) error { return nil }}},
		BuildInfo{Version: "test"})
}

func do(t *testing.T, h http.Handler, method, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s: response is not JSON: %v\n%s", method, path, err, rec.Body.String())
		}
	}
	return rec, body
}

// Liveness must not depend on Prometheus: a liveness probe that fails during a
// dependency outage would restart a healthy optimizer and turn an outage into a
// crash loop.
func TestHealthzIgnoresDependencies(t *testing.T) {
	svc := newTestService(t, []runtimeObject{ns("app")}, newFakeSource(100, 1e8), testPolicy())
	srv := NewServer(svc, quiet(), metrics.New(),
		[]NamedCheck{{Name: "prometheus", Check: func(context.Context) error {
			return errors.New("prometheus is down")
		}}},
		BuildInfo{Version: "test"})
	rec, body := do(t, srv.Handler(), "GET", "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200 even with a failing dependency", rec.Code)
	}
	if body["status"] != "ok" {
		t.Errorf("healthz body = %v", body)
	}
}

// Readiness must fail before the first cycle: otherwise an empty result set
// would be served as though the cluster had nothing to optimise.
func TestReadyzFailsBeforeFirstAnalysis(t *testing.T) {
	svc := newTestService(t, []runtimeObject{ns("app")}, newFakeSource(100, 1e8), testPolicy())
	srv := newTestServer(t, svc)
	rec, body := do(t, srv.Handler(), "GET", "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d, want 503 before the first cycle", rec.Code)
	}
	if body["analysis"] != "pending" {
		t.Errorf("expected analysis=pending, got %v", body["analysis"])
	}

	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec2, body2 := do(t, srv.Handler(), "GET", "/readyz")
	if rec2.Code != http.StatusOK {
		t.Errorf("readyz = %d after analysis, want 200 (body: %v)", rec2.Code, body2)
	}
}

func TestReadyzFailsOnUnreachableDependency(t *testing.T) {
	svc := newTestService(t, []runtimeObject{ns("app")}, newFakeSource(100, 1e8), testPolicy())
	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(svc, quiet(), metrics.New(),
		[]NamedCheck{{Name: "prometheus", Check: func(context.Context) error {
			return errors.New("connection refused")
		}}},
		BuildInfo{Version: "test"})
	rec, _ := do(t, srv.Handler(), "GET", "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d, want 503 when a dependency is down", rec.Code)
	}
}

func TestRecommendationsEndpoint(t *testing.T) {
	svc := newTestService(t,
		[]runtimeObject{ns("app"), deployment("app", "api", 3, "2", "4Gi"), healthyPod("app", "api")},
		newFakeSource(200, 500*float64(model.BytesPerMi)), testPolicy())
	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, svc)

	rec, body := do(t, srv.Handler(), "GET", "/api/v1/recommendations")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := body["count"].(float64); got != 1 {
		t.Errorf("count = %v, want 1", got)
	}
	recs := body["recommendations"].([]any)
	first := recs[0].(map[string]any)
	containers := first["containers"].([]any)
	cpu := containers[0].(map[string]any)["cpu"].(map[string]any)

	// Values must be rendered as Kubernetes quantities, so they are directly
	// comparable with what is written in a manifest.
	if cpu["current"] != "2" {
		t.Errorf("current CPU = %v, want \"2\"", cpu["current"])
	}
	if cpu["recommended"] == nil || cpu["recommended"] == "" {
		t.Error("a DECREASE must carry a recommended value")
	}
	if cpu["reason"] == "" {
		t.Error("every recommendation must carry a reason")
	}
	if _, ok := cpu["observed"]; !ok {
		t.Error("the observed statistics must be included so the advice can be audited")
	}
}

func TestRecommendationsFilters(t *testing.T) {
	svc := newTestService(t,
		[]runtimeObject{
			ns("app"), ns("other"),
			deployment("app", "api", 1, "2", "4Gi"), healthyPod("app", "api"),
			deployment("other", "web", 1, "2", "4Gi"), healthyPod("other", "web"),
		},
		newFakeSource(200, 500*float64(model.BytesPerMi)), testPolicy())
	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := newTestServer(t, svc).Handler()

	_, body := do(t, h, "GET", "/api/v1/recommendations?namespace=app")
	if got := body["count"].(float64); got != 1 {
		t.Errorf("namespace filter: count = %v, want 1", got)
	}
	_, body = do(t, h, "GET", "/api/v1/recommendations?decision=DECREASE")
	if got := body["count"].(float64); got != 2 {
		t.Errorf("decision filter: count = %v, want 2", got)
	}
	_, body = do(t, h, "GET", "/api/v1/recommendations?decision=BLOCKED")
	if got := body["count"].(float64); got != 0 {
		t.Errorf("decision=BLOCKED: count = %v, want 0", got)
	}
	_, body = do(t, h, "GET", "/api/v1/recommendations?limit=1")
	if got := body["count"].(float64); got != 1 {
		t.Errorf("limit: count = %v, want 1", got)
	}
	if got := body["total_matching"].(float64); got != 2 {
		t.Errorf("limit must still report the total matching count, got %v", got)
	}
	_, body = do(t, h, "GET", "/api/v1/recommendations?min_savings_usd=100000")
	if got := body["count"].(float64); got != 0 {
		t.Errorf("min_savings filter: count = %v, want 0", got)
	}
}

func TestRecommendationsRejectsBadParameters(t *testing.T) {
	svc := newTestService(t, []runtimeObject{ns("app")}, newFakeSource(100, 1e8), testPolicy())
	_ = svc.Analyze(context.Background())
	h := newTestServer(t, svc).Handler()
	for _, path := range []string{
		"/api/v1/recommendations?min_savings_usd=abc",
		"/api/v1/recommendations?limit=-1",
		"/api/v1/recommendations?limit=xyz",
	} {
		rec, _ := do(t, h, "GET", path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestSingleRecommendationEndpoint(t *testing.T) {
	svc := newTestService(t,
		[]runtimeObject{ns("app"), deployment("app", "api", 1, "2", "4Gi"), healthyPod("app", "api")},
		newFakeSource(200, 500e6), testPolicy())
	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := newTestServer(t, svc).Handler()

	rec, body := do(t, h, "GET", "/api/v1/recommendations/app/api")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body["name"] != "api" {
		t.Errorf("name = %v", body["name"])
	}

	rec404, body404 := do(t, h, "GET", "/api/v1/recommendations/app/missing")
	if rec404.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec404.Code)
	}
	// The 404 message must be actionable: "not found" alone leaves the operator
	// unsure whether the workload is excluded or merely not yet analysed.
	if msg, _ := body404["error"].(string); len(msg) < 40 {
		t.Errorf("404 message should explain the possible causes, got %q", msg)
	}
}

func TestWorkloadsEndpoint(t *testing.T) {
	svc := newTestService(t,
		[]runtimeObject{ns("app"), deployment("app", "api", 2, "2", "4Gi"), oomPod("app", "api")},
		newFakeSource(200, 500e6), testPolicy())
	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, body := do(t, newTestServer(t, svc).Handler(), "GET", "/api/v1/workloads")
	wls := body["workloads"].([]any)
	if len(wls) != 1 {
		t.Fatalf("got %d workloads", len(wls))
	}
	c := wls[0].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if c["cpu_request"] != "2" || c["memory_request"] != "4Gi" {
		t.Errorf("declared resources rendered as %v / %v", c["cpu_request"], c["memory_request"])
	}
	if got := c["oom_kills"].(float64); got != 1 {
		t.Errorf("oom_kills = %v, want 1", got)
	}
}

// The summary must always carry the cost-model caveat: a savings figure
// presented without it invites being read as a cloud bill.
func TestSummaryCarriesCostModelCaveat(t *testing.T) {
	svc := newTestService(t,
		[]runtimeObject{ns("app"), deployment("app", "api", 1, "2", "4Gi"), healthyPod("app", "api")},
		newFakeSource(200, 500e6), testPolicy())
	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, body := do(t, newTestServer(t, svc).Handler(), "GET", "/api/v1/summary")
	caveat, ok := body["cost_model_caveat"].(string)
	if !ok || caveat == "" {
		t.Error("the summary must state the cost model's limits")
	}
	if _, ok := body["summary"]; !ok {
		t.Error("missing summary")
	}
}

func TestPolicyEndpointExposesConfiguration(t *testing.T) {
	svc := newTestService(t, []runtimeObject{ns("app")}, newFakeSource(100, 1e8), testPolicy())
	_ = svc.Analyze(context.Background())
	_, body := do(t, newTestServer(t, svc).Handler(), "GET", "/api/v1/policy")
	if body["policy_id"] == "" {
		t.Error("the policy endpoint must expose the policy ID so a recommendation is traceable")
	}
	p := body["policy"].(map[string]any)
	if p["cpu_strategy"] == nil || p["memory_strategy"] == nil {
		t.Errorf("policy is missing its strategies: %v", p)
	}
}

func TestUnsupportedMethodReturns405(t *testing.T) {
	svc := newTestService(t, []runtimeObject{ns("app")}, newFakeSource(100, 1e8), testPolicy())
	h := newTestServer(t, svc).Handler()
	req := httptest.NewRequest("POST", "/api/v1/recommendations", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to a GET-only route = %d, want 405", rec.Code)
	}
}

// Recommendations must never be cached: a cached recommendation describes a
// cluster that has since changed.
func TestResponsesAreUncacheable(t *testing.T) {
	svc := newTestService(t, []runtimeObject{ns("app")}, newFakeSource(100, 1e8), testPolicy())
	_ = svc.Analyze(context.Background())
	rec, _ := do(t, newTestServer(t, svc).Handler(), "GET", "/api/v1/recommendations")
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestMetricsEndpointExposesOptimizerMetrics(t *testing.T) {
	svc, m := newTestServiceWithMetrics(t,
		[]runtimeObject{ns("app"), deployment("app", "api", 1, "2", "4Gi"), healthyPod("app", "api")},
		newFakeSource(200, 500e6), testPolicy())
	if err := svc.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(svc, quiet(), m, nil, BuildInfo{})
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.MetricsHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"optimizer_workloads_discovered",
		"optimizer_recommendations_generated_total",
		"optimizer_analysis_duration_seconds",
		"optimizer_estimated_monthly_savings_usd",
	} {
		if !containsStr(body, want) {
			t.Errorf("metrics output is missing %s", want)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
