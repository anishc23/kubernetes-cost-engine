// Package api exposes the optimizer's analysis over HTTP and contains the
// analysis service that drives it.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/cost"
	"github.com/anishc23/k8s-cost-optimizer/internal/kube"
	"github.com/anishc23/k8s-cost-optimizer/internal/metrics"
	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/internal/promapi"
	"github.com/anishc23/k8s-cost-optimizer/internal/recommender"
)

// Service runs analysis cycles and holds the most recent results.
//
// Results are served from an in-memory snapshot rather than computed per
// request. A request-time computation would issue a burst of Prometheus queries
// on every dashboard refresh, which turns a monitoring dashboard into a load
// generator against the monitoring system it depends on.
type Service struct {
	discoverer kube.Discoverer
	source     promapi.Source
	engine     *recommender.Engine
	estimator  *cost.Estimator
	log        *slog.Logger
	metrics    *metrics.Metrics

	// Concurrency bounds simultaneous Prometheus queries.
	concurrency int
	// queryStep is the resolution requested from Prometheus.
	queryStep time.Duration

	mu       sync.RWMutex
	snapshot Snapshot
}

// Snapshot is an immutable view of one completed analysis cycle.
//
// Serving a whole snapshot rather than individual fields means a reader can
// never observe a half-updated state: recommendations and the summary computed
// from them are always mutually consistent.
type Snapshot struct {
	GeneratedAt     time.Time                      `json:"generated_at"`
	Workloads       []model.Workload               `json:"-"`
	Recommendations []model.WorkloadRecommendation `json:"recommendations"`
	Summary         cost.Summary                   `json:"summary"`
	PolicyID        string                         `json:"policy_id"`
	// Errors records per-workload failures. A cycle that could not analyse some
	// workloads still publishes the rest, but the omissions must be visible
	// rather than silently reducing the reported cost.
	Errors []WorkloadError `json:"errors,omitempty"`
	// Ready is false until the first cycle completes, so that readiness can
	// distinguish "no savings found" from "no analysis has run yet".
	Ready bool `json:"ready"`
}

// WorkloadError records a workload that could not be analysed.
type WorkloadError struct {
	Workload string `json:"workload"`
	Error    string `json:"error"`
}

// Options configures the service.
type Options struct {
	Concurrency int
	QueryStep   time.Duration
}

// NewService builds the analysis service.
func NewService(
	d kube.Discoverer, s promapi.Source, e *recommender.Engine, est *cost.Estimator,
	opts Options, log *slog.Logger, m *metrics.Metrics,
) *Service {
	if opts.Concurrency < 1 {
		opts.Concurrency = 8
	}
	if opts.QueryStep <= 0 {
		opts.QueryStep = time.Minute
	}
	return &Service{
		discoverer: d, source: s, engine: e, estimator: est,
		log: log, metrics: m,
		concurrency: opts.Concurrency, queryStep: opts.QueryStep,
	}
}

// Snapshot returns the most recent completed analysis.
func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

// Run executes analysis cycles until the context is cancelled.
//
// The first cycle runs immediately rather than after one interval, so that a
// freshly started optimizer becomes useful within seconds instead of minutes.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if err := s.Analyze(ctx); err != nil {
		s.log.Error("initial analysis failed", "error", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.log.Info("analysis loop stopping")
			return
		case <-t.C:
			if err := s.Analyze(ctx); err != nil {
				s.log.Error("analysis cycle failed", "error", err)
			}
		}
	}
}

// Analyze runs one complete cycle: discover, collect, recommend, price.
func (s *Service) Analyze(ctx context.Context) error {
	start := time.Now()
	defer func() {
		if s.metrics != nil {
			s.metrics.AnalysisDuration.Observe(time.Since(start).Seconds())
		}
	}()

	workloads, err := s.discoverer.Discover(ctx)
	if err != nil {
		if s.metrics != nil {
			s.metrics.AnalysisErrors.Inc()
		}
		return fmt.Errorf("discover workloads: %w", err)
	}
	s.log.Info("discovered workloads", "count", len(workloads))

	recs, errs := s.analyzeAll(ctx, workloads)

	// Deterministic ordering makes the API output stable across cycles, which
	// matters for anything that diffs it and for dashboards that would otherwise
	// reshuffle on every refresh.
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Namespace != recs[j].Namespace {
			return recs[i].Namespace < recs[j].Namespace
		}
		return recs[i].Name < recs[j].Name
	})

	summary := cost.Summarize(recs, s.estimator.Model().Name)
	snap := Snapshot{
		GeneratedAt:     time.Now().UTC(),
		Workloads:       workloads,
		Recommendations: recs,
		Summary:         summary,
		PolicyID:        s.engine.Policy().PolicyID(),
		Errors:          errs,
		Ready:           true,
	}

	s.mu.Lock()
	s.snapshot = snap
	s.mu.Unlock()

	s.publishMetrics(snap)
	s.log.Info("analysis complete",
		"workloads", len(recs), "errors", len(errs),
		"current_monthly_usd", summary.CurrentMonthlyUSD,
		"savings_monthly_usd", summary.MonthlySavingsUSD,
		"duration", time.Since(start).String())
	return nil
}

// analyzeAll collects usage and produces recommendations, bounded by the
// configured concurrency.
func (s *Service) analyzeAll(ctx context.Context, workloads []model.Workload) ([]model.WorkloadRecommendation, []WorkloadError) {
	type result struct {
		rec model.WorkloadRecommendation
		err error
		key string
	}
	results := make([]result, len(workloads))
	sem := make(chan struct{}, s.concurrency)
	var wg sync.WaitGroup

	for i := range workloads {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			w := workloads[i]
			results[i].key = w.Key()
			filled, err := s.collect(ctx, w)
			if err != nil {
				results[i].err = err
				return
			}
			rec := s.engine.Recommend(filled)
			s.estimator.Estimate(&rec)
			results[i].rec = rec
		}(i)
	}
	wg.Wait()

	var recs []model.WorkloadRecommendation
	var errs []WorkloadError
	for _, r := range results {
		if r.err != nil {
			// A workload that could not be analysed is reported, not dropped
			// silently: an invisible omission would understate the cluster's
			// cost and hide a broken metrics pipeline.
			errs = append(errs, WorkloadError{Workload: r.key, Error: r.err.Error()})
			continue
		}
		recs = append(recs, r.rec)
	}
	return recs, errs
}

// collect fills a workload's containers with their usage series.
func (s *Service) collect(ctx context.Context, w model.Workload) (model.Workload, error) {
	window := s.engine.Policy().ObservationWindow
	podRegex := kube.PodRegexFor(w)

	for i := range w.Containers {
		c := &w.Containers[i]
		q := promapi.Query{
			Namespace: w.Namespace,
			PodRegex:  podRegex,
			Container: c.Name,
			Window:    window,
			Step:      s.queryStep,
		}
		cpu, err := s.source.CPUUsage(ctx, q)
		if err != nil {
			return w, fmt.Errorf("cpu usage for %s/%s: %w", w.Key(), c.Name, err)
		}
		mem, err := s.source.MemoryUsage(ctx, q)
		if err != nil {
			return w, fmt.Errorf("memory usage for %s/%s: %w", w.Key(), c.Name, err)
		}
		c.CPU = cpu
		c.Memory = mem

		// Throttling is supplementary evidence: a failure to read it must not
		// fail the whole workload, since the CPU and memory series are what the
		// recommendation actually depends on.
		if ratio, available, err := s.source.Throttling(ctx, q); err != nil {
			s.log.Debug("throttling query failed", "workload", w.Key(), "container", c.Name, "error", err)
		} else if available {
			ev := w.Evidence[c.Name]
			ev.CPUThrottledRatio = ratio
			if w.Evidence == nil {
				w.Evidence = map[string]model.RestartEvidence{}
			}
			w.Evidence[c.Name] = ev
		}
	}
	return w, nil
}

func (s *Service) publishMetrics(snap Snapshot) {
	if s.metrics == nil {
		return
	}
	s.metrics.LastAnalysisTimestamp.Set(float64(snap.GeneratedAt.Unix()))
	s.metrics.EstimatedMonthlyCost.Set(snap.Summary.CurrentMonthlyUSD)
	s.metrics.EstimatedMonthlySavings.Set(snap.Summary.MonthlySavingsUSD)
	for _, r := range snap.Recommendations {
		for _, c := range r.Containers {
			s.metrics.RecommendationsGenerated.WithLabelValues("cpu", string(c.CPU.Decision)).Inc()
			s.metrics.RecommendationsGenerated.WithLabelValues("memory", string(c.Memory.Decision)).Inc()
			for _, g := range c.CPU.Gates {
				s.metrics.GatesFired.WithLabelValues(g, "cpu").Inc()
			}
			for _, g := range c.Memory.Gates {
				s.metrics.GatesFired.WithLabelValues(g, "memory").Inc()
			}
		}
	}
}

// Find returns the recommendation for one workload.
func (s *Service) Find(namespace, name string) (model.WorkloadRecommendation, bool) {
	snap := s.Snapshot()
	for _, r := range snap.Recommendations {
		if r.Namespace == namespace && r.Name == name {
			return r, true
		}
	}
	return model.WorkloadRecommendation{}, false
}
