package experiment

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/cost"
	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/internal/recommender"
	"github.com/anishc23/k8s-cost-optimizer/internal/simulator"
)

// Runner executes an experiment configuration.
type Runner struct {
	cfg       Config
	log       *slog.Logger
	estimator *cost.Estimator
	// Concurrency is the number of conditions evaluated in parallel. Conditions
	// are independent by construction — each generates its own trace from its own
	// seed — so parallelism cannot change results, only the time taken. This is
	// verified by TestParallelismDoesNotChangeResults.
	Concurrency int
	// ToolVersion and Git metadata are recorded in the provenance block.
	ToolVersion string
	GitCommit   string
	GitDirty    bool
}

// NewRunner validates the configuration and builds the cost estimator.
func NewRunner(cfg Config, log *slog.Logger) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	instance := cfg.CostInstance
	if instance == "" {
		instance = "m5.xlarge"
	}
	it, err := cost.InstanceByName(instance)
	if err != nil {
		return nil, err
	}
	share := cfg.CPUCostShare
	if share == 0 {
		share = cost.CPUCostShare
	}
	m, err := cost.DeriveModel(it, share)
	if err != nil {
		return nil, err
	}
	est, err := cost.NewEstimator(m)
	if err != nil {
		return nil, err
	}
	return &Runner{
		cfg:         cfg,
		log:         log,
		estimator:   est,
		Concurrency: runtime.NumCPU(),
		ToolVersion: "dev",
	}, nil
}

// condition is one fully specified point in the experiment matrix.
type condition struct {
	class    simulator.Class
	seed     int64
	cpuStrat string
	memStrat string
	cpuSF    float64
	memSF    float64
	window   time.Duration
}

// expand enumerates the matrix in a deterministic order, so that the row order
// of an output file is a function of the configuration alone.
func (r *Runner) expand() []condition {
	var out []condition
	memSFs := r.cfg.memorySafetyFactors()
	for _, class := range r.cfg.classes() {
		for s := 0; s < r.cfg.Seeds; s++ {
			seed := r.cfg.BaseSeed + int64(s)*1_000_003 // large prime stride
			for _, cpuStrat := range r.cfg.CPUStrategies {
				memStrats := r.cfg.MemoryStrategies
				if r.cfg.UnifiedStrategy {
					// Ablation C: treat both resources identically.
					memStrats = []string{cpuStrat}
				}
				for _, memStrat := range memStrats {
					for _, w := range r.cfg.ObservationWindows.Std() {
						for _, cpuSF := range r.cfg.SafetyFactors {
							if memSFs == nil {
								out = append(out, condition{class, seed, cpuStrat, memStrat, cpuSF, cpuSF, w})
								continue
							}
							for _, memSF := range memSFs {
								out = append(out, condition{class, seed, cpuStrat, memStrat, cpuSF, memSF, w})
							}
						}
					}
				}
			}
		}
	}
	return out
}

// Run executes every condition and returns the complete result.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	prov := NewProvenance(r.cfg, r.ToolVersion, r.GitCommit, r.GitDirty, r.estimator.Model().Name)
	conditions := r.expand()
	r.log.Info("experiment starting",
		"name", r.cfg.Name, "conditions", len(conditions),
		"concurrency", r.Concurrency, "config_hash", prov.ConfigHash)

	records := make([]Record, len(conditions))
	errs := make([]error, len(conditions))

	sem := make(chan struct{}, max(1, r.Concurrency))
	var wg sync.WaitGroup
	var done int64
	var mu sync.Mutex

	for i, c := range conditions {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		wg.Add(1)
		go func(i int, c condition) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rec, err := r.evaluate(c, prov.ConfigHash)
			if err != nil {
				errs[i] = err
				return
			}
			records[i] = rec
			mu.Lock()
			done++
			if done%500 == 0 {
				r.log.Info("experiment progress", "completed", done, "total", len(conditions))
			}
			mu.Unlock()
		}(i, c)
	}
	wg.Wait()

	// A single failed condition invalidates the run: a results file with silent
	// holes is worse than no results file, because the gaps are invisible in
	// aggregate statistics.
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("condition %d (%s/%s/%s): %w",
				i, conditions[i].class, conditions[i].cpuStrat, conditions[i].memStrat, err)
		}
	}

	prov.FinishedAt = time.Now().UTC()
	prov.DurationMS = prov.FinishedAt.Sub(prov.StartedAt).Milliseconds()
	prov.Records = len(records)
	r.log.Info("experiment complete",
		"name", r.cfg.Name, "records", len(records), "duration", prov.FinishedAt.Sub(prov.StartedAt))
	return &Result{Provenance: prov, Config: r.cfg, Records: records}, nil
}

// evaluate runs one condition end to end.
//
// The sequence is the methodological core of the project:
//
//  1. Generate a full demand trace from the class spec and seed.
//  2. Split it: the fitting window (what the engine may observe) and the
//     held-out evaluation horizon (what it is scored on).
//  3. Censor the fitting window by the declared configuration, producing the
//     series a monitoring system would actually have recorded.
//  4. Run the production engine on the censored fitting window.
//  5. Replay the recommendation against the *uncensored* horizon demand.
//  6. Price both configurations and compute metrics.
//
// Step 5 is what the evaluation turns on. The engine is scored against demand it
// never saw, over a period it did not fit to.
func (r *Runner) evaluate(c condition, runID string) (Record, error) {
	spec, err := simulator.SpecByClass(c.class, r.cfg.TraceDuration.D(), r.cfg.Step.D(), c.seed)
	if err != nil {
		return Record{}, err
	}
	full, err := simulator.Generate(spec)
	if err != nil {
		return Record{}, err
	}

	fit, horizon, err := splitTrace(full, c.window, r.cfg.EvaluationHorizon.D())
	if err != nil {
		return Record{}, err
	}

	replayCfg := r.cfg.replayConfig()
	policy := r.policyFor(c)
	engine, err := recommender.NewEngine(policy)
	if err != nil {
		return Record{}, err
	}

	// The engine sees only the censored fitting window.
	w := simulator.AsWorkload(fit, replayCfg)
	rec := engine.Recommend(w)
	r.estimator.Estimate(&rec)
	cr := rec.Containers[0]

	// Resolve the configuration that would actually be deployed. An
	// INSUFFICIENT_DATA decision means no change was proposed, so the declared
	// request stands — which is what gets replayed.
	cpuReq := cr.CPU.Target
	if cr.CPU.Decision == model.DecisionInsufficientData {
		cpuReq = cr.CPU.Current
	}
	memReq := cr.Memory.Target
	if cr.Memory.Decision == model.DecisionInsufficientData {
		memReq = cr.Memory.Current
	}

	// Score against the held-out horizon.
	outcome := simulator.Replay(horizon, cpuReq, memReq, replayCfg)

	// Cost is computed on the same basis for both configurations, using the
	// production estimator.
	currentMonthly := r.estimator.Model().MonthlyUSD(
		model.Millicores(spec.DeclaredCPU), model.Bytes(spec.DeclaredMemory))
	recommendedMonthly := r.estimator.Model().MonthlyUSD(
		model.Millicores(cpuReq), model.Bytes(memReq))

	feas := r.cfg.Feasibility
	if feas == (FeasibilityConstraint{}) {
		feas = DefaultFeasibility()
	}

	out := Record{
		Experiment:             r.cfg.Name,
		RunID:                  runID,
		WorkloadClass:          string(c.class),
		WorkloadName:           spec.Name,
		Seed:                   c.seed,
		CPUStrategy:            c.cpuStrat,
		MemoryStrategy:         c.memStrat,
		CPUSafetyFactor:        c.cpuSF,
		MemorySafetyFactor:     c.memSF,
		ObservationWindowHours: c.window.Hours(),
		EvaluationHorizonHours: r.cfg.EvaluationHorizon.D().Hours(),
		StepSeconds:            r.cfg.Step.D().Seconds(),
		PolicyID:               policy.PolicyID(),
		OOMProtectionEnabled:   !r.cfg.DisableOOMProtection,
		UsageExceedsEnabled:    !r.cfg.DisableUsageExceeds,
		FloorsEnabled:          !r.cfg.DisableFloors,
		MinChangeEnabled:       !r.cfg.DisableMinChange,
		RoundingEnabled:        !r.cfg.DisableRounding,
		UnifiedStrategy:        r.cfg.UnifiedStrategy,
		DeclaredCPUMilli:       spec.DeclaredCPU,
		DeclaredMemoryBytes:    spec.DeclaredMemory,
		CPUDecision:            string(cr.CPU.Decision),
		MemoryDecision:         string(cr.Memory.Decision),
		CPURisk:                string(cr.CPU.Risk),
		MemoryRisk:             string(cr.Memory.Risk),
		RecommendedCPUMilli:    cpuReq,
		RecommendedMemoryBytes: memReq,
		RawCPUMilli:            cr.CPU.RawTarget,
		RawMemoryBytes:         cr.Memory.RawTarget,
		CPUGates:               gatesString(cr.CPU.Gates),
		MemoryGates:            gatesString(cr.Memory.Gates),
		Metrics:                ComputeMetrics(horizon, outcome, currentMonthly, recommendedMonthly, feas),
	}
	out.fillObserved(cr)
	// Ground truth comes from the *horizon*, the period being scored, not from
	// the fitting window.
	out.fillGroundTruth(horizon.GroundTruth)

	if r.cfg.StabilityRecomputations > 1 {
		st, err := r.measureStability(full, c, policy, replayCfg)
		if err != nil {
			return Record{}, err
		}
		out.Stability = st
	}
	return out, nil
}

// measureStability recomputes the recommendation over successive, advancing
// windows and summarises how much it moved.
//
// Each recomputation slides the window forward by one evaluation horizon,
// mimicking a recommender that runs on a schedule. What is measured is the
// volatility an operator would actually experience, not the sensitivity of a
// statistic to a synthetic perturbation.
func (r *Runner) measureStability(
	full simulator.Trace, c condition, policy recommender.Policy, replayCfg simulator.Config,
) (*StabilityMetrics, error) {
	engine, err := recommender.NewEngine(policy)
	if err != nil {
		return nil, err
	}
	n := r.cfg.StabilityRecomputations
	cpuVals := make([]float64, 0, n)
	memVals := make([]float64, 0, n)

	step := full.Spec.Step
	windowSamples := int(c.window / step)
	strideSamples := int(r.cfg.EvaluationHorizon.D() / step)
	total := full.CPUDemand.Len()

	for i := 0; i < n; i++ {
		// Window i ends strideSamples further into the trace than window i-1,
		// mimicking a recommender that runs on a fixed schedule. What is measured
		// is the volatility an operator would actually experience.
		hi := windowSamples + i*strideSamples
		if hi > total {
			break
		}
		sub, err := subTrace(full, hi-windowSamples, hi)
		if err != nil {
			return nil, err
		}
		w := simulator.AsWorkload(sub, replayCfg)
		rec := engine.Recommend(w)
		cr := rec.Containers[0]
		if cr.CPU.Decision != model.DecisionInsufficientData {
			cpuVals = append(cpuVals, cr.CPU.Target)
		}
		if cr.Memory.Decision != model.DecisionInsufficientData {
			memVals = append(memVals, cr.Memory.Target)
		}
	}
	minChange := policy.MinRelativeChange
	cpuStab := ComputeStability(cpuVals, minChange)
	memStab := ComputeStability(memVals, minChange)
	// The reported figure is the worse of the two resources: a workload whose
	// memory recommendation is stable but whose CPU recommendation oscillates is
	// still operationally disruptive, because either change forces a rollout.
	combined := cpuStab
	if memStab.MedianAbsLog2Ratio > combined.MedianAbsLog2Ratio {
		combined = memStab
	}
	return &combined, nil
}

// policyFor builds the engine policy for a condition, applying ablation flags.
func (r *Runner) policyFor(c condition) recommender.Policy {
	p := recommender.DefaultPolicy()
	p.CPUStrategy = c.cpuStrat
	p.MemoryStrategy = c.memStrat
	p.CPUSafetyFactor = c.cpuSF
	p.MemorySafetyFactor = c.memSF
	p.ObservationWindow = c.window
	if r.cfg.PercentileMethod != "" {
		p.PercentileMethod = r.cfg.PercentileMethod
	}
	// Sufficiency thresholds are scaled to the step so that a one-hour window is
	// not rejected merely for being short: the question RQ6 asks is what a short
	// window *produces*, which cannot be answered if short windows are refused.
	// The floor of 20 samples remains, because below that no percentile is
	// meaningful at any window length.
	p.MinSamples = 20
	p.MinDuration = 30 * time.Minute

	if r.cfg.DisableOOMProtection {
		p.OOMProtection = false
	}
	if r.cfg.DisableUsageExceeds {
		p.UsageExceedsRequestPercentile = ""
	}
	if r.cfg.DisableFloors {
		p.CPUFloorMilli = 0
		p.MemoryFloorBytes = 0
	}
	if r.cfg.DisableMinChange {
		p.MinRelativeChange = 0
	}
	if r.cfg.DisableRounding {
		p.Rounding = false
	}
	return p
}

// splitTrace divides a trace into a fitting window and a held-out horizon.
//
// The split is expressed in half-open sample index ranges rather than
// durations, because duration arithmetic on a sampled series invites off-by-one
// errors at both boundaries — and a boundary sample shared between the fitting
// window and the evaluation horizon would leak a sample of the future into the
// past, which is exactly what this split exists to prevent.
//
// The horizon is anchored at the end of the trace, so every window length is
// scored on the same future. Without that anchoring, RQ6 would be comparing
// different futures rather than different histories.
func splitTrace(full simulator.Trace, window, horizon time.Duration) (fit, eval simulator.Trace, err error) {
	step := full.Spec.Step
	n := full.CPUDemand.Len()
	horizonSamples := int(horizon / step)
	windowSamples := int(window / step)
	if horizonSamples < 1 || windowSamples < 1 {
		return fit, eval, fmt.Errorf(
			"window %s and horizon %s must each span at least one %s step", window, horizon, step)
	}
	horizonStart := n - horizonSamples
	fitStart := horizonStart - windowSamples
	if fitStart < 0 {
		return fit, eval, fmt.Errorf(
			"window %s plus horizon %s needs %d samples but the trace has %d",
			window, horizon, windowSamples+horizonSamples, n)
	}
	fit, err = subTrace(full, fitStart, horizonStart)
	if err != nil {
		return fit, eval, err
	}
	eval, err = subTrace(full, horizonStart, n)
	return fit, eval, err
}

// subTrace extracts the half-open sample range [lo, hi) and recomputes ground
// truth for it.
//
// Recomputation is essential: scoring a two-hour horizon against a seven-day
// maximum would make every recommendation look under-provisioned, and the
// resulting "finding" would be an artefact of the split.
func subTrace(full simulator.Trace, lo, hi int) (simulator.Trace, error) {
	if lo < 0 {
		lo = 0
	}
	if hi > full.CPUDemand.Len() {
		hi = full.CPUDemand.Len()
	}
	if lo >= hi {
		return simulator.Trace{}, fmt.Errorf("sub-trace range [%d,%d) selects no samples", lo, hi)
	}
	step := full.Spec.Step
	spec := full.Spec
	// Each sample represents one step of elapsed time, so the duration covered
	// by n samples is n*step. This is the convention the demand integral in
	// simulator.Replay uses, and the two must agree or throttled core-seconds
	// would not divide correctly by demanded core-seconds.
	spec.Duration = time.Duration(hi-lo) * step
	sub := simulator.Trace{
		Spec: spec,
		CPUDemand: model.Series{
			Resource: model.ResourceCPU, Step: step,
			Samples: full.CPUDemand.Samples[lo:hi],
		},
		MemoryDemand: model.Series{
			Resource: model.ResourceMemory, Step: step,
			Samples: full.MemoryDemand.Samples[lo:hi],
		},
	}
	sub.GroundTruth = simulator.ComputeGroundTruth(sub)
	return sub, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
