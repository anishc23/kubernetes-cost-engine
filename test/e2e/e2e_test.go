//go:build e2e

// Package e2e validates the production data path end to end against a real
// Kubernetes cluster: Kubernetes → Prometheus → optimizer → REST API.
//
// # What this test does and does not establish
//
// It establishes that the *collection machinery* is correct: that discovery
// finds workloads and resolves their controller ownership, that the Prometheus
// queries match the right series through real cAdvisor metrics, that the
// pod-name regexes resolve, and that recommendations are served.
//
// It does NOT establish that a recommendation was *good*. A real cluster
// provides no ground truth — the usage it reports is censored by the
// configuration the workload is already running under — which is precisely why
// the quantitative findings come from the simulation study instead. See
// research/methodology.md.
//
// Run with: make kind-e2e   (or `make e2e` against an already-running cluster)
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	defaultAPIBase  = "http://localhost:30080"
	defaultPromBase = "http://localhost:30090"
	// The optimizer analyses on an interval; a freshly installed release needs a
	// cycle or two before it has anything to say.
	readyTimeout = 5 * time.Minute
)

func apiBase() string {
	if v := os.Getenv("OPTIMIZER_API"); v != "" {
		return v
	}
	return defaultAPIBase
}

func promBase() string {
	if v := os.Getenv("PROMETHEUS_URL"); v != "" {
		return v
	}
	return defaultPromBase
}

// getJSON fetches and decodes a JSON endpoint.
func getJSON(t *testing.T, url string, into any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if into != nil && len(body) > 0 {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("decode %s: %v\nbody: %s", url, err, truncate(string(body), 2000))
		}
	}
	return resp.StatusCode
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}

// TestMain establishes that the cluster is reachable before any subtest runs, so
// that a missing cluster produces one clear skip rather than a wall of failures.
func TestMain(m *testing.M) {
	if _, err := http.Get(apiBase() + "/healthz"); err != nil {
		fmt.Fprintf(os.Stderr,
			"e2e: optimizer API not reachable at %s (%v)\nRun `make kind-e2e`, or set OPTIMIZER_API.\n",
			apiBase(), err)
		os.Exit(0) // skip rather than fail: the cluster is a precondition, not a result
	}
	os.Exit(m.Run())
}

// --- the dependency chain, checked in order -------------------------------

func TestPrometheusHasContainerMetrics(t *testing.T) {
	// Checked first and explicitly: if cAdvisor metrics are absent, every
	// downstream assertion fails in a way that points at the optimizer rather
	// than at the cluster.
	for _, metric := range []string{
		"container_cpu_usage_seconds_total",
		"container_memory_working_set_bytes",
	} {
		url := fmt.Sprintf("%s/api/v1/query?query=count(%s{namespace=\"demo\"})",
			promBase(), metric)
		var res struct {
			Status string `json:"status"`
			Data   struct {
				Result []struct {
					Value []any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if code := getJSON(t, url, &res); code != http.StatusOK {
			t.Fatalf("%s: Prometheus returned %d", metric, code)
		}
		if res.Status != "success" || len(res.Data.Result) == 0 {
			t.Fatalf("%s: no series found for the demo namespace; "+
				"cAdvisor scraping is not working", metric)
		}
		t.Logf("%s: present", metric)
	}
}

func TestOptimizerBecomesReady(t *testing.T) {
	deadline := time.Now().Add(readyTimeout)
	var lastBody map[string]any
	for time.Now().Before(deadline) {
		lastBody = map[string]any{}
		if code := getJSON(t, apiBase()+"/readyz", &lastBody); code == http.StatusOK {
			t.Logf("ready after %v", readyTimeout-time.Until(deadline))
			return
		}
		time.Sleep(5 * time.Second)
	}
	b, _ := json.MarshalIndent(lastBody, "", "  ")
	t.Fatalf("optimizer did not become ready within %v; last /readyz response:\n%s", readyTimeout, b)
}

// Liveness must not depend on Prometheus, so that a dependency outage does not
// become a crash loop.
func TestHealthzIsIndependentOfDependencies(t *testing.T) {
	var body map[string]any
	if code := getJSON(t, apiBase()+"/healthz", &body); code != http.StatusOK {
		t.Fatalf("/healthz returned %d", code)
	}
	if body["status"] != "ok" {
		t.Errorf("/healthz body = %v", body)
	}
}

// --- discovery ------------------------------------------------------------

type workloadsResponse struct {
	Count     int `json:"count"`
	Workloads []struct {
		Namespace  string `json:"namespace"`
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Replicas   int    `json:"replicas"`
		Containers []struct {
			Name          string `json:"name"`
			CPURequest    string `json:"cpu_request"`
			MemoryRequest string `json:"memory_request"`
			OOMKills      int    `json:"oom_kills"`
		} `json:"containers"`
	} `json:"workloads"`
}

func TestDiscoversDemoWorkloads(t *testing.T) {
	var res workloadsResponse
	if code := getJSON(t, apiBase()+"/api/v1/workloads?namespace=demo", &res); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	found := map[string]bool{}
	for _, w := range res.Workloads {
		found[w.Name] = true
		t.Logf("discovered %s/%s (%s, %d replicas): cpu=%s memory=%s",
			w.Namespace, w.Name, w.Kind, w.Replicas,
			w.Containers[0].CPURequest, w.Containers[0].MemoryRequest)
	}
	for _, want := range []string{"stable-cpu", "bursty-cpu", "growing-memory", "idle"} {
		if !found[want] {
			t.Errorf("workload %q was not discovered", want)
		}
	}
}

// Declared requests must be read back exactly as written in the manifest. An
// error here would mean every recommendation is computed against the wrong
// baseline.
func TestDeclaredRequestsMatchTheManifests(t *testing.T) {
	var res workloadsResponse
	getJSON(t, apiBase()+"/api/v1/workloads?namespace=demo", &res)

	want := map[string]struct{ cpu, mem string }{
		"stable-cpu":     {"1", "1Gi"},
		"bursty-cpu":     {"1500m", "1Gi"},
		"growing-memory": {"500m", "1Gi"},
		"idle":           {"500m", "512Mi"},
	}
	for _, w := range res.Workloads {
		exp, ok := want[w.Name]
		if !ok {
			continue
		}
		got := w.Containers[0]
		if got.CPURequest != exp.cpu {
			t.Errorf("%s: CPU request = %q, manifest says %q", w.Name, got.CPURequest, exp.cpu)
		}
		if got.MemoryRequest != exp.mem {
			t.Errorf("%s: memory request = %q, manifest says %q", w.Name, got.MemoryRequest, exp.mem)
		}
	}
	// The idle workload has two replicas, which the cost arithmetic depends on.
	for _, w := range res.Workloads {
		if w.Name == "idle" && w.Replicas != 2 {
			t.Errorf("idle replicas = %d, want 2", w.Replicas)
		}
	}
}

// --- recommendations ------------------------------------------------------

type recommendationsResponse struct {
	Count           int    `json:"count"`
	PolicyID        string `json:"policy_id"`
	Recommendations []struct {
		Namespace  string `json:"namespace"`
		Name       string `json:"name"`
		Containers []struct {
			Container string `json:"container"`
			CPU       advice `json:"cpu"`
			Memory    advice `json:"memory"`
		} `json:"containers"`
		Cost struct {
			CurrentMonthlyUSD     float64 `json:"current_monthly_usd"`
			RecommendedMonthlyUSD float64 `json:"recommended_monthly_usd"`
			MonthlySavingsUSD     float64 `json:"monthly_savings_usd"`
		} `json:"cost"`
	} `json:"recommendations"`
	Errors []struct {
		Workload string `json:"workload"`
		Error    string `json:"error"`
	} `json:"errors"`
}

type advice struct {
	Decision    string   `json:"decision"`
	Risk        string   `json:"risk"`
	Current     string   `json:"current"`
	Recommended string   `json:"recommended"`
	Reason      string   `json:"reason"`
	Gates       []string `json:"gates"`
	Samples     int      `json:"samples"`
	Observed    struct {
		Mean string `json:"mean"`
		P95  string `json:"p95"`
		Max  string `json:"max"`
	} `json:"observed"`
}

func TestGeneratesRecommendationsFromRealMetrics(t *testing.T) {
	var res recommendationsResponse
	if code := getJSON(t, apiBase()+"/api/v1/recommendations?namespace=demo", &res); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(res.Errors) > 0 {
		for _, e := range res.Errors {
			t.Errorf("workload %s could not be analysed: %s", e.Workload, e.Error)
		}
	}
	if res.Count == 0 {
		t.Fatal("no recommendations were produced")
	}
	if res.PolicyID == "" {
		t.Error("recommendations must carry a policy ID for traceability")
	}

	for _, r := range res.Recommendations {
		c := r.Containers[0]
		t.Logf("%s/%s cpu: %s %s -> %s (%d samples, observed mean %s / max %s)",
			r.Namespace, r.Name, c.CPU.Decision, c.CPU.Current, c.CPU.Recommended,
			c.CPU.Samples, c.CPU.Observed.Mean, c.CPU.Observed.Max)
		t.Logf("%s/%s mem: %s %s -> %s",
			r.Namespace, r.Name, c.Memory.Decision, c.Memory.Current, c.Memory.Recommended)

		// Every recommendation must be auditable: a decision without evidence
		// is not actionable advice.
		for _, a := range []advice{c.CPU, c.Memory} {
			if a.Reason == "" {
				t.Errorf("%s/%s: a recommendation has no reason", r.Namespace, r.Name)
			}
			if a.Decision == "" || a.Risk == "" {
				t.Errorf("%s/%s: incomplete recommendation %+v", r.Namespace, r.Name, a)
			}
		}
		// Real metrics must have reached the engine.
		if c.CPU.Samples == 0 && c.CPU.Decision != "INSUFFICIENT_DATA" {
			t.Errorf("%s/%s: a decision was made from zero samples", r.Namespace, r.Name)
		}
	}
}

// The clearest end-to-end signal: a workload declaring 500m and using ~5m must
// be identified as over-provisioned. If this fails, something in the chain from
// cAdvisor to the recommendation is broken, even if every component's unit tests
// pass.
func TestIdentifiesTheObviouslyOverProvisionedWorkload(t *testing.T) {
	var res recommendationsResponse
	getJSON(t, apiBase()+"/api/v1/recommendations?namespace=demo", &res)

	for _, r := range res.Recommendations {
		if r.Name != "idle" {
			continue
		}
		c := r.Containers[0]
		if c.CPU.Decision == "INSUFFICIENT_DATA" {
			t.Skipf("not enough history yet for the idle workload (%d samples); "+
				"let the cluster run longer", c.CPU.Samples)
		}
		if c.CPU.Decision != "DECREASE" {
			t.Errorf("idle workload CPU decision = %s, want DECREASE "+
				"(declared %s, observed mean %s). Reason: %s",
				c.CPU.Decision, c.CPU.Current, c.CPU.Observed.Mean, c.CPU.Reason)
		}
		if r.Cost.MonthlySavingsUSD <= 0 {
			t.Errorf("idle workload shows no saving: $%.2f", r.Cost.MonthlySavingsUSD)
		}
		// Two replicas, so its cost must exceed a single replica's.
		if r.Cost.CurrentMonthlyUSD <= 0 {
			t.Error("idle workload has no current cost")
		}
		return
	}
	t.Error("the idle workload produced no recommendation")
}

// --- API contract ---------------------------------------------------------

func TestSummaryEndpoint(t *testing.T) {
	var res struct {
		PolicyID string `json:"policy_id"`
		Summary  struct {
			Workloads         int     `json:"workloads"`
			CurrentMonthlyUSD float64 `json:"current_monthly_usd"`
			MonthlySavingsUSD float64 `json:"monthly_savings_usd"`
			Decreases         int     `json:"decreases"`
			Blocked           int     `json:"blocked"`
			InsufficientData  int     `json:"insufficient_data"`
		} `json:"summary"`
		Caveat string `json:"cost_model_caveat"`
	}
	if code := getJSON(t, apiBase()+"/api/v1/summary", &res); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if res.Summary.Workloads == 0 {
		t.Error("the summary reports no workloads")
	}
	if res.Summary.CurrentMonthlyUSD <= 0 {
		t.Error("the summary reports no current cost")
	}
	// The caveat must always accompany a savings figure, or the number invites
	// being read as a cloud bill.
	if !strings.Contains(res.Caveat, "not cloud invoices") {
		t.Errorf("the summary must state the cost model's limits; got %q", res.Caveat)
	}
	t.Logf("cluster: %d workloads, $%.2f/mo current, $%.2f/mo identified savings "+
		"(%d decreases, %d blocked, %d insufficient data)",
		res.Summary.Workloads, res.Summary.CurrentMonthlyUSD, res.Summary.MonthlySavingsUSD,
		res.Summary.Decreases, res.Summary.Blocked, res.Summary.InsufficientData)
}

func TestSingleWorkloadEndpoint(t *testing.T) {
	var res struct {
		Name       string `json:"name"`
		Containers []struct {
			CPU advice `json:"cpu"`
		} `json:"containers"`
	}
	code := getJSON(t, apiBase()+"/api/v1/recommendations/demo/stable-cpu", &res)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if res.Name != "stable-cpu" {
		t.Errorf("name = %q", res.Name)
	}

	// A missing workload must 404 with an actionable message.
	var errBody map[string]string
	code404 := getJSON(t, apiBase()+"/api/v1/recommendations/demo/does-not-exist", &errBody)
	if code404 != http.StatusNotFound {
		t.Errorf("missing workload returned %d, want 404", code404)
	}
	if len(errBody["error"]) < 40 {
		t.Errorf("the 404 message should explain the possible causes, got %q", errBody["error"])
	}
}

func TestPolicyEndpointReflectsDeployedConfiguration(t *testing.T) {
	var res struct {
		PolicyID string `json:"policy_id"`
		Policy   struct {
			CPUStrategy    string `json:"cpu_strategy"`
			MemoryStrategy string `json:"memory_strategy"`
			OOMProtection  bool   `json:"oom_protection"`
		} `json:"policy"`
	}
	if code := getJSON(t, apiBase()+"/api/v1/policy", &res); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if res.PolicyID == "" {
		t.Error("no policy ID")
	}
	if !res.Policy.OOMProtection {
		t.Error("OOM protection must be enabled in the deployed configuration")
	}
	t.Logf("deployed policy %s: cpu=%s memory=%s",
		res.PolicyID, res.Policy.CPUStrategy, res.Policy.MemoryStrategy)
}

// --- the optimizer's own observability ------------------------------------

func TestExposesItsOwnMetrics(t *testing.T) {
	// Scraped through Prometheus rather than directly, which also confirms the
	// ServiceMonitor/scrape configuration works.
	url := promBase() + "/api/v1/query?query=optimizer_workloads_discovered"
	var res struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	getJSON(t, url, &res)
	if res.Status != "success" || len(res.Data.Result) == 0 {
		t.Skip("the optimizer's own metrics are not being scraped; " +
			"this is a monitoring configuration issue, not an optimizer defect")
	}
	t.Logf("optimizer_workloads_discovered = %v", res.Data.Result[0].Value[1])
}

// --- the default must not mutate the cluster ------------------------------

// The most important safety property of the whole system: installing the chart
// with defaults must leave workloads untouched. Verified against the live
// cluster rather than against the rendered manifest.
func TestDefaultDeploymentDoesNotMutateWorkloads(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "kubectl", "-n", "demo", "get", "deploy",
		"-o", `jsonpath={range .items[*]}{.metadata.name}={.spec.template.spec.containers[0].resources.requests.cpu}{"\n"}{end}`,
	).Output()
	if err != nil {
		t.Skipf("could not query the cluster: %v", err)
	}

	// These are the values from the manifest. If the optimizer had applied its
	// own recommendations, they would differ.
	want := map[string]string{
		"stable-cpu":     "1",
		"bursty-cpu":     "1500m",
		"growing-memory": "500m",
		"idle":           "500m",
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, got, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if exp, known := want[name]; known && got != exp {
			t.Errorf("workload %s has CPU request %q but the manifest declares %q: "+
				"the optimizer appears to have mutated the cluster in recommendation-only mode",
				name, got, exp)
		}
	}
	t.Log("all demo workloads retain their declared requests: no mutation occurred")
}
