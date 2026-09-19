package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/anishc23/k8s-cost-optimizer/internal/metrics"
	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/pkg/quantity"
)

// Server serves the versioned REST API.
type Server struct {
	svc     *Service
	log     *slog.Logger
	metrics *metrics.Metrics
	// readinessDeps are checked by /readyz.
	readinessDeps []NamedCheck
	// version is reported by /api/v1/version so a deployed build is identifiable.
	version BuildInfo
}

// BuildInfo identifies the running build.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
}

// NamedCheck is a readiness dependency.
type NamedCheck struct {
	Name  string
	Check func(context.Context) error
}

// NewServer builds the API server.
func NewServer(svc *Service, log *slog.Logger, m *metrics.Metrics, deps []NamedCheck, build BuildInfo) *Server {
	return &Server{svc: svc, log: log, metrics: m, readinessDeps: deps, version: build}
}

// Handler returns the API mux.
//
// Routes are registered with Go 1.22 method patterns so that an unsupported
// method returns 405 rather than being silently handled, and the versioned
// prefix is explicit on every route: a client that pins /api/v1 must never be
// broken by a later /api/v2.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	mux.Handle("GET /api/v1/version", s.instrument("version", s.handleVersion))
	mux.Handle("GET /api/v1/workloads", s.instrument("workloads", s.handleWorkloads))
	mux.Handle("GET /api/v1/recommendations", s.instrument("recommendations", s.handleRecommendations))
	mux.Handle("GET /api/v1/recommendations/{namespace}/{workload}",
		s.instrument("recommendation", s.handleRecommendation))
	mux.Handle("GET /api/v1/summary", s.instrument("summary", s.handleSummary))
	mux.Handle("GET /api/v1/policy", s.instrument("policy", s.handlePolicy))

	return mux
}

// MetricsHandler serves the optimizer's own Prometheus metrics.
func (s *Server) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{
		// Errors in one collector must not blank the whole scrape.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// instrument wraps a handler with metrics and access logging.
func (s *Server) instrument(route string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h(rec, r)
		elapsed := time.Since(start)
		if s.metrics != nil {
			s.metrics.APIRequests.WithLabelValues(route, statusClass(rec.status)).Inc()
			s.metrics.APIDuration.WithLabelValues(route).Observe(elapsed.Seconds())
		}
		s.log.Debug("api request",
			"route", route, "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", elapsed.Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func statusClass(code int) string { return fmt.Sprintf("%dxx", code/100) }

// --- health ---------------------------------------------------------------

// handleHealthz reports process liveness only.
//
// It deliberately does not check dependencies. A liveness probe that fails when
// Prometheus is unreachable would cause Kubernetes to restart a perfectly
// healthy optimizer, turning a dependency outage into a crash loop that makes
// recovery slower. Dependency health belongs in readiness.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports whether the service can serve meaningful answers.
//
// Readiness requires both that dependencies are reachable and that at least one
// analysis cycle has completed. Without the second condition the API would
// report an empty result set as though it were a cluster with nothing to
// optimise.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	type depStatus struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	out := struct {
		Status       string      `json:"status"`
		Analysis     string      `json:"analysis"`
		Dependencies []depStatus `json:"dependencies"`
	}{Status: "ready"}

	for _, d := range s.readinessDeps {
		st := depStatus{Name: d.Name, Status: "ok"}
		if err := d.Check(ctx); err != nil {
			st.Status = "error"
			st.Error = err.Error()
			out.Status = "not ready"
		}
		out.Dependencies = append(out.Dependencies, st)
	}

	snap := s.svc.Snapshot()
	if snap.Ready {
		out.Analysis = "complete"
	} else {
		out.Analysis = "pending"
		out.Status = "not ready"
	}

	code := http.StatusOK
	if out.Status != "ready" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, out)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.version)
}

// --- workloads ------------------------------------------------------------

// WorkloadView is the API representation of a discovered workload.
//
// Resource values are rendered as Kubernetes quantity strings rather than raw
// numbers, because the consumer of this endpoint is a human or a dashboard, and
// "1536Mi" is directly comparable to what is written in a manifest while
// 1610612736 is not.
type WorkloadView struct {
	Namespace  string          `json:"namespace"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	Replicas   int32           `json:"replicas"`
	Containers []ContainerView `json:"containers"`
}

// ContainerView is one container's declared resources and observed evidence.
type ContainerView struct {
	Name           string  `json:"name"`
	CPURequest     string  `json:"cpu_request"`
	MemoryRequest  string  `json:"memory_request"`
	CPULimit       string  `json:"cpu_limit,omitempty"`
	MemoryLimit    string  `json:"memory_limit,omitempty"`
	Restarts       int     `json:"restarts"`
	OOMKills       int     `json:"oom_kills"`
	ThrottledRatio float64 `json:"cpu_throttled_ratio"`
}

func (s *Server) handleWorkloads(w http.ResponseWriter, r *http.Request) {
	snap := s.svc.Snapshot()
	ns := r.URL.Query().Get("namespace")

	views := make([]WorkloadView, 0, len(snap.Workloads))
	for _, wl := range snap.Workloads {
		if ns != "" && wl.Namespace != ns {
			continue
		}
		v := WorkloadView{
			Namespace: wl.Namespace, Name: wl.Name,
			Kind: string(wl.Kind), Replicas: wl.Replicas,
		}
		for _, c := range wl.Containers {
			cv := ContainerView{
				Name:          c.Name,
				CPURequest:    quantity.CPUString(c.Declared.CPURequest),
				MemoryRequest: quantity.MemoryString(c.Declared.MemoryRequest),
			}
			if c.Declared.CPULimit != nil {
				cv.CPULimit = quantity.CPUString(*c.Declared.CPULimit)
			}
			if c.Declared.MemoryLimit != nil {
				cv.MemoryLimit = quantity.MemoryString(*c.Declared.MemoryLimit)
			}
			if ev, ok := wl.EvidenceFor(c.Name); ok {
				cv.Restarts, cv.OOMKills, cv.ThrottledRatio = ev.Restarts, ev.OOMKills, ev.CPUThrottledRatio
			}
			v.Containers = append(v.Containers, cv)
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": snap.GeneratedAt,
		"count":        len(views),
		"workloads":    views,
	})
}

// --- recommendations ------------------------------------------------------

// RecommendationView renders a recommendation for human and dashboard use.
type RecommendationView struct {
	Namespace   string                        `json:"namespace"`
	Name        string                        `json:"name"`
	Kind        string                        `json:"kind"`
	Replicas    int32                         `json:"replicas"`
	GeneratedAt time.Time                     `json:"generated_at"`
	PolicyID    string                        `json:"policy_id"`
	Containers  []ContainerRecommendationView `json:"containers"`
	Cost        model.CostEstimate            `json:"cost"`
}

// ContainerRecommendationView renders one container's advice.
type ContainerRecommendationView struct {
	Container string             `json:"container"`
	CPU       ResourceAdviceView `json:"cpu"`
	Memory    ResourceAdviceView `json:"memory"`
}

// ResourceAdviceView is the operator-facing form of a recommendation.
//
// It carries the decision, the values, the reason and the evidence together,
// because an advisory system whose reasoning cannot be inspected will not be
// acted on — and should not be.
type ResourceAdviceView struct {
	Decision string `json:"decision"`
	Risk     string `json:"risk"`
	Current  string `json:"current"`
	// Recommended is omitted for INSUFFICIENT_DATA, where no value was produced.
	Recommended string   `json:"recommended,omitempty"`
	Reason      string   `json:"reason"`
	Gates       []string `json:"gates,omitempty"`

	Observed ObservedView `json:"observed"`

	SavingsFraction float64 `json:"savings_fraction"`
	Strategy        string  `json:"strategy"`
	SafetyFactor    float64 `json:"safety_factor"`
	Samples         int     `json:"samples"`
	WindowHours     float64 `json:"window_hours"`
}

// ObservedView is the usage summary a recommendation was derived from.
type ObservedView struct {
	Mean       string  `json:"mean"`
	P95        string  `json:"p95"`
	P99        string  `json:"p99"`
	Max        string  `json:"max"`
	Burstiness float64 `json:"burstiness"`
}

func (s *Server) handleRecommendations(w http.ResponseWriter, r *http.Request) {
	snap := s.svc.Snapshot()
	q := r.URL.Query()
	ns := q.Get("namespace")
	decision := strings.ToUpper(q.Get("decision"))
	risk := strings.ToUpper(q.Get("risk"))

	// minSavings lets a dashboard show only material opportunities without
	// transferring every workload in a large cluster.
	var minSavings float64
	if v := q.Get("min_savings_usd"); v != "" {
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("min_savings_usd %q is not a number", v))
			return
		}
		minSavings = parsed
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit %q must be a non-negative integer", v))
			return
		}
		limit = parsed
	}

	views := make([]RecommendationView, 0, len(snap.Recommendations))
	for _, rec := range snap.Recommendations {
		if ns != "" && rec.Namespace != ns {
			continue
		}
		if rec.Cost.MonthlySavingsUSD < minSavings {
			continue
		}
		if decision != "" && !matchesDecision(rec, decision) {
			continue
		}
		if risk != "" && !matchesRisk(rec, risk) {
			continue
		}
		views = append(views, toView(rec))
	}

	// Sorted by savings descending: the most valuable opportunity first is what
	// an operator opening this endpoint is looking for.
	sort.SliceStable(views, func(i, j int) bool {
		return views[i].Cost.MonthlySavingsUSD > views[j].Cost.MonthlySavingsUSD
	})
	total := len(views)
	if limit > 0 && limit < len(views) {
		views = views[:limit]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at":    snap.GeneratedAt,
		"policy_id":       snap.PolicyID,
		"count":           len(views),
		"total_matching":  total,
		"recommendations": views,
		"errors":          snap.Errors,
	})
}

func (s *Server) handleRecommendation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("workload")
	rec, ok := s.svc.Find(ns, name)
	if !ok {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("no recommendation for %s/%s; it may not exist, may be excluded from discovery, or may not yet have been analysed", ns, name))
		return
	}
	writeJSON(w, http.StatusOK, toView(rec))
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	snap := s.svc.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": snap.GeneratedAt,
		"policy_id":    snap.PolicyID,
		"summary":      snap.Summary,
		"errors":       len(snap.Errors),
		"cost_model_caveat": "Allocation-based estimate: it prices reserved capacity, not cloud invoices. " +
			"Realising a saving requires the freed capacity to let the cluster run fewer nodes. See docs/cost-model.md.",
	})
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	p := s.svc.engine.Policy()
	writeJSON(w, http.StatusOK, map[string]any{
		"policy_id": p.PolicyID(),
		"policy":    p,
	})
}

func matchesDecision(rec model.WorkloadRecommendation, decision string) bool {
	for _, c := range rec.Containers {
		if string(c.CPU.Decision) == decision || string(c.Memory.Decision) == decision {
			return true
		}
	}
	return false
}

func matchesRisk(rec model.WorkloadRecommendation, risk string) bool {
	for _, c := range rec.Containers {
		if string(c.CPU.Risk) == risk || string(c.Memory.Risk) == risk {
			return true
		}
	}
	return false
}

func toView(rec model.WorkloadRecommendation) RecommendationView {
	v := RecommendationView{
		Namespace: rec.Namespace, Name: rec.Name, Kind: string(rec.Kind),
		Replicas: rec.Replicas, GeneratedAt: rec.GeneratedAt,
		PolicyID: rec.PolicyID, Cost: rec.Cost,
	}
	for _, c := range rec.Containers {
		v.Containers = append(v.Containers, ContainerRecommendationView{
			Container: c.Container,
			CPU:       adviceView(c.CPU, true),
			Memory:    adviceView(c.Memory, false),
		})
	}
	return v
}

func adviceView(r model.ResourceRecommendation, isCPU bool) ResourceAdviceView {
	fmtVal := func(v float64) string {
		if isCPU {
			return quantity.CPUString(model.Millicores(v))
		}
		return quantity.MemoryString(model.Bytes(v))
	}
	out := ResourceAdviceView{
		Decision: string(r.Decision),
		Risk:     string(r.Risk),
		Current:  fmtVal(r.Current),
		Reason:   r.Reason,
		Gates:    r.Gates,
		Observed: ObservedView{
			Mean: fmtVal(r.Stats.Mean), P95: fmtVal(r.Stats.P95),
			P99: fmtVal(r.Stats.P99), Max: fmtVal(r.Stats.Max),
			Burstiness: r.Stats.Burstiness,
		},
		Strategy:     r.Strategy,
		SafetyFactor: r.SafetyFactor,
		Samples:      r.Stats.Samples,
		WindowHours:  r.Stats.WindowDuration.Hours(),
	}
	if r.Decision != model.DecisionInsufficientData {
		out.Recommended = fmtVal(r.Target)
		if r.Current > 0 {
			out.SavingsFraction = (r.Current - r.Target) / r.Current
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Responses are explicitly marked uncacheable: a cached recommendation is a
	// stale description of a cluster that has since changed, and acting on one
	// is exactly the failure mode the freshness metrics exist to catch.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
