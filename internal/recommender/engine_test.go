package recommender

import (
	"math"
	"testing"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/internal/stats"
)

// --- helpers ---------------------------------------------------------------

// constantSeries builds a series with the given values at a 30s step, ending
// "now" so that recency checks behave as they would in production.
func series(kind model.ResourceKind, step time.Duration, values ...float64) model.Series {
	end := time.Now()
	samples := make([]model.Sample, len(values))
	for i, v := range values {
		samples[i] = model.Sample{
			Timestamp: end.Add(-time.Duration(len(values)-1-i) * step),
			Value:     v,
		}
	}
	return model.Series{Resource: kind, Samples: samples, Step: step}
}

func repeat(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// sufficientPolicy relaxes the data thresholds so that tests can use short
// series to exercise a specific rule without every case tripping the
// sufficiency gate first.
func sufficientPolicy() Policy {
	p := DefaultPolicy()
	p.MinSamples = 10
	p.MinDuration = time.Minute
	p.ObservationWindow = 24 * time.Hour
	return p
}

func mustEngine(t *testing.T, p Policy) *Engine {
	t.Helper()
	e, err := NewEngine(p)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func workload(cpu, mem model.Series, cpuReq model.Millicores, memReq model.Bytes, ev *model.RestartEvidence) model.Workload {
	w := model.Workload{
		Namespace: "test", Name: "wl", Kind: model.KindDeployment, Replicas: 1,
		Containers: []model.Container{{
			Name:     "app",
			Declared: model.ResourceRequests{CPURequest: cpuReq, MemoryRequest: memReq},
			CPU:      cpu, Memory: mem,
		}},
	}
	if ev != nil {
		w.Evidence = map[string]model.RestartEvidence{"app": *ev}
	}
	return w
}

// --- policy validation ----------------------------------------------------

func TestPolicyValidation(t *testing.T) {
	reg := NewRegistry(stats.LinearInterpolation)
	cases := []struct {
		name  string
		mut   func(*Policy)
		valid bool
	}{
		{"default", func(*Policy) {}, true},
		{"unknown cpu strategy", func(p *Policy) { p.CPUStrategy = "p42" }, false},
		{"unknown memory strategy", func(p *Policy) { p.MemoryStrategy = "magic" }, false},
		{"cpu safety below 1", func(p *Policy) { p.CPUSafetyFactor = 0.9 }, false},
		{"memory safety below 1", func(p *Policy) { p.MemorySafetyFactor = 0.5 }, false},
		{"zero window", func(p *Policy) { p.ObservationWindow = 0 }, false},
		{"coverage above 1", func(p *Policy) { p.MinCoverage = 1.5 }, false},
		{"min change 1.0", func(p *Policy) { p.MinRelativeChange = 1.0 }, false},
		{"bad exceeds percentile", func(p *Policy) { p.UsageExceedsRequestPercentile = "p50" }, false},
		{"empty exceeds percentile disables gate", func(p *Policy) { p.UsageExceedsRequestPercentile = "" }, true},
	}
	for _, c := range cases {
		p := DefaultPolicy()
		c.mut(&p)
		err := p.Validate(reg)
		if c.valid && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.valid && err == nil {
			t.Errorf("%s: expected validation error", c.name)
		}
	}
}

// A safety factor below 1.0 must be rejected rather than silently clamped: it is
// far more likely a configuration mistake than an intention, and clamping would
// hide it.
func TestNewEngineRejectsUnsafePolicy(t *testing.T) {
	p := DefaultPolicy()
	p.MemorySafetyFactor = 0.8
	if _, err := NewEngine(p); err == nil {
		t.Fatal("expected NewEngine to reject a memory safety factor below 1.0")
	}
}

// The policy ID must change whenever any field changes, since results are traced
// back to configurations through it.
func TestPolicyIDIsContentAddressed(t *testing.T) {
	a := DefaultPolicy()
	b := DefaultPolicy()
	if a.PolicyID() != b.PolicyID() {
		t.Error("identical policies must share an ID")
	}
	b.CPUSafetyFactor = 1.20
	if a.PolicyID() == b.PolicyID() {
		t.Error("differing policies must not share an ID")
	}
	c := DefaultPolicy()
	c.ObservationWindow = time.Hour
	if a.PolicyID() == c.PolicyID() {
		t.Error("observation window must affect the policy ID")
	}
}

// --- core behaviour -------------------------------------------------------

func TestDecreaseOnOverProvisionedStableWorkload(t *testing.T) {
	p := sufficientPolicy()
	e := mustEngine(t, p)
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(200, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(300*float64(model.BytesPerMi), 120)...)
	ev := model.RestartEvidence{} // present, clean
	rec := e.Recommend(workload(cpu, mem, 2000, model.Bytes(2048*model.BytesPerMi), &ev))

	c := rec.Containers[0]
	if c.CPU.Decision != model.DecisionDecrease {
		t.Errorf("CPU decision = %s, want DECREASE (reason: %s)", c.CPU.Decision, c.CPU.Reason)
	}
	// p95 of a constant 200m series is 200m; x1.15 = 230m, rounded up to 230m.
	if c.CPU.Target < 200 || c.CPU.Target > 260 {
		t.Errorf("CPU target %v outside the expected 200-260m band", c.CPU.Target)
	}
	if c.Memory.Decision != model.DecisionDecrease {
		t.Errorf("memory decision = %s, want DECREASE (reason: %s)", c.Memory.Decision, c.Memory.Reason)
	}
	if c.CPU.Reason == "" || c.Memory.Reason == "" {
		t.Error("every recommendation must carry a reason")
	}
}

// Recommendations must be reproducible: the same input and policy must give the
// same output, because experiment results are aggregated across repeated runs.
func TestRecommendIsDeterministic(t *testing.T) {
	e := mustEngine(t, sufficientPolicy())
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(200, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(300*float64(model.BytesPerMi), 120)...)
	ev := model.RestartEvidence{}
	w := workload(cpu, mem, 2000, model.Bytes(2048*model.BytesPerMi), &ev)
	a := e.Recommend(w)
	b := e.Recommend(w)
	if a.Containers[0].CPU.Target != b.Containers[0].CPU.Target ||
		a.Containers[0].Memory.Target != b.Containers[0].Memory.Target {
		t.Error("recommendations must be deterministic for identical input")
	}
}

// --- gates ----------------------------------------------------------------

func TestInsufficientDataBlocksAnyRecommendation(t *testing.T) {
	e := mustEngine(t, DefaultPolicy()) // MinSamples 60, MinDuration 1h
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(50, 5)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(100e6, 5)...)
	ev := model.RestartEvidence{}
	rec := e.Recommend(workload(cpu, mem, 2000, 4e9, &ev))
	c := rec.Containers[0]
	for _, r := range []model.ResourceRecommendation{c.CPU, c.Memory} {
		if r.Decision != model.DecisionInsufficientData {
			t.Errorf("%s: decision = %s, want INSUFFICIENT_DATA", r.Resource, r.Decision)
		}
		if r.Target != 0 {
			t.Errorf("%s: INSUFFICIENT_DATA must not carry a target, got %v", r.Resource, r.Target)
		}
		if r.Risk != model.RiskUnknown {
			t.Errorf("%s: risk = %s, want UNKNOWN", r.Resource, r.Risk)
		}
	}
}

// Low coverage must be caught even when the series spans the full window: a
// series with long gaps describes only a fraction of the period.
func TestLowCoverageIsInsufficientEvenWithLongSpan(t *testing.T) {
	p := sufficientPolicy()
	p.MinSamples = 10
	p.MinCoverage = 0.8
	e := mustEngine(t, p)

	// 20 samples at a declared 30s step, but spread over 6 hours: coverage ~3%.
	end := time.Now()
	samples := make([]model.Sample, 20)
	for i := range samples {
		samples[i] = model.Sample{
			Timestamp: end.Add(-time.Duration(20-1-i) * 18 * time.Minute),
			Value:     100,
		}
	}
	cpu := model.Series{Resource: model.ResourceCPU, Samples: samples, Step: 30 * time.Second}
	ev := model.RestartEvidence{}
	rec := e.Recommend(workload(cpu, cpu, 2000, 4e9, &ev))
	if got := rec.Containers[0].CPU.Decision; got != model.DecisionInsufficientData {
		t.Errorf("decision = %s, want INSUFFICIENT_DATA for a sparse series", got)
	}
	if cov := rec.Containers[0].CPU.DataSufficiency.Coverage; cov > 0.2 {
		t.Errorf("coverage %.3f should be low for a sparse series", cov)
	}
}

// The central safety property: a workload with OOM history must never be handed
// a memory reduction, however over-provisioned its observed usage looks.
func TestOOMHistoryBlocksMemoryReduction(t *testing.T) {
	e := mustEngine(t, sufficientPolicy())
	last := time.Now().Add(-2 * time.Hour)
	ev := model.RestartEvidence{Restarts: 4, OOMKills: 3, LastOOM: &last}
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(100, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(200*float64(model.BytesPerMi), 120)...)
	rec := e.Recommend(workload(cpu, mem, 2000, model.Bytes(4096*model.BytesPerMi), &ev))

	c := rec.Containers[0]
	if c.Memory.Decision != model.DecisionBlocked {
		t.Fatalf("memory decision = %s, want BLOCKED (reason: %s)", c.Memory.Decision, c.Memory.Reason)
	}
	if c.Memory.Target != c.Memory.Current {
		t.Errorf("blocked memory target %v must equal current %v", c.Memory.Target, c.Memory.Current)
	}
	if c.Memory.Risk != model.RiskHigh {
		t.Errorf("memory risk = %s, want HIGH with OOM history", c.Memory.Risk)
	}
	// CPU must still be right-sized: the asymmetry is the whole point, and an
	// OOM is not evidence about CPU.
	if c.CPU.Decision != model.DecisionDecrease {
		t.Errorf("CPU decision = %s: OOM history must not block CPU reduction", c.CPU.Decision)
	}
	// The raw statistical target must still be reported, so the effect of the
	// gate is measurable rather than invisible.
	if c.Memory.RawTarget >= c.Memory.Current {
		t.Errorf("RawTarget %v should show the reduction the gate withheld", c.Memory.RawTarget)
	}
}

// Absent evidence must make the engine more conservative, not less.
func TestMissingEvidenceBlocksMemoryReduction(t *testing.T) {
	e := mustEngine(t, sufficientPolicy())
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(100, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(200*float64(model.BytesPerMi), 120)...)
	rec := e.Recommend(workload(cpu, mem, 2000, model.Bytes(4096*model.BytesPerMi), nil)) // no evidence

	c := rec.Containers[0]
	if c.Memory.Decision != model.DecisionBlocked {
		t.Errorf("memory decision = %s, want BLOCKED when no evidence was collected", c.Memory.Decision)
	}
	if c.CPU.Decision != model.DecisionDecrease {
		t.Errorf("CPU decision = %s: missing OOM evidence must not block CPU", c.CPU.Decision)
	}
}

// An OOM outside the relevance window is not evidence about current behaviour.
func TestStaleOOMDoesNotBlock(t *testing.T) {
	p := sufficientPolicy()
	p.OOMLookbackRelevance = 24 * time.Hour
	e := mustEngine(t, p)
	old := time.Now().Add(-30 * 24 * time.Hour)
	ev := model.RestartEvidence{Restarts: 1, OOMKills: 1, LastOOM: &old}
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(100, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(200*float64(model.BytesPerMi), 120)...)
	rec := e.Recommend(workload(cpu, mem, 2000, model.Bytes(4096*model.BytesPerMi), &ev))
	if got := rec.Containers[0].Memory.Decision; got != model.DecisionDecrease {
		t.Errorf("memory decision = %s, want DECREASE for an OOM outside the relevance window (reason: %s)",
			got, rec.Containers[0].Memory.Reason)
	}
}

// OOM history must never block an *increase*: the gate exists to prevent unsafe
// reductions, not to freeze a workload that needs more memory.
func TestOOMHistoryAllowsIncrease(t *testing.T) {
	e := mustEngine(t, sufficientPolicy())
	last := time.Now().Add(-time.Hour)
	ev := model.RestartEvidence{Restarts: 2, OOMKills: 2, LastOOM: &last}
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(100, 120)...)
	// Observed usage far above the current request.
	mem := series(model.ResourceMemory, 30*time.Second, repeat(900*float64(model.BytesPerMi), 120)...)
	rec := e.Recommend(workload(cpu, mem, 2000, model.Bytes(512*model.BytesPerMi), &ev))
	c := rec.Containers[0]
	if c.Memory.Decision != model.DecisionIncrease {
		t.Errorf("memory decision = %s, want INCREASE despite OOM history (reason: %s)",
			c.Memory.Decision, c.Memory.Reason)
	}
	if c.Memory.Risk != model.RiskLow {
		t.Errorf("an increase reduces risk by construction; got risk %s", c.Memory.Risk)
	}
}

func TestUsageExceedingRequestBlocksReduction(t *testing.T) {
	e := mustEngine(t, sufficientPolicy())
	ev := model.RestartEvidence{}
	// p99 usage above the current request, but a low mean so a mean-based policy
	// would still propose a reduction.
	vals := append(repeat(100, 115), repeat(1200, 5)...)
	p := sufficientPolicy()
	p.CPUStrategy = "mean"
	p.CPUSafetyFactor = 1.0
	e = mustEngine(t, p)
	cpu := series(model.ResourceCPU, 30*time.Second, vals...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(200e6, 120)...)
	rec := e.Recommend(workload(cpu, mem, 1000, 4e9, &ev))
	c := rec.Containers[0]
	if c.CPU.Decision != model.DecisionBlocked {
		t.Errorf("CPU decision = %s, want BLOCKED when p99 exceeds the request (reason: %s)",
			c.CPU.Decision, c.CPU.Reason)
	}
}

func TestFloorRaisesTinyTargets(t *testing.T) {
	p := sufficientPolicy()
	p.CPUFloorMilli = 50
	p.MemoryFloorBytes = 64 * float64(model.BytesPerMi)
	e := mustEngine(t, p)
	ev := model.RestartEvidence{}
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(2, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(4*float64(model.BytesPerMi), 120)...)
	rec := e.Recommend(workload(cpu, mem, 500, model.Bytes(512*model.BytesPerMi), &ev))
	c := rec.Containers[0]
	if c.CPU.Target < 50 {
		t.Errorf("CPU target %v is below the 50m floor", c.CPU.Target)
	}
	if c.Memory.Target < 64*float64(model.BytesPerMi) {
		t.Errorf("memory target %v is below the 64Mi floor", c.Memory.Target)
	}
	foundFloor := false
	for _, g := range c.CPU.Gates {
		if g == "floor" {
			foundFloor = true
		}
	}
	if !foundFloor {
		t.Errorf("expected the floor gate to be recorded, got gates %v", c.CPU.Gates)
	}
}

func TestMinChangeSuppressesMarginalAdjustments(t *testing.T) {
	p := sufficientPolicy()
	p.MinRelativeChange = 0.10
	p.CPUStrategy = "p95"
	p.CPUSafetyFactor = 1.0
	e := mustEngine(t, p)
	ev := model.RestartEvidence{}
	// p95 = 960m against a 1000m request: a 4% change, below the threshold.
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(960, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(200e6, 120)...)
	rec := e.Recommend(workload(cpu, mem, 1000, 4e9, &ev))
	if got := rec.Containers[0].CPU.Decision; got != model.DecisionNoChange {
		t.Errorf("CPU decision = %s, want NO_CHANGE for a 4%% adjustment (reason: %s)",
			got, rec.Containers[0].CPU.Reason)
	}
}

func TestInstabilityGateBlocksBurstyReductions(t *testing.T) {
	p := sufficientPolicy()
	p.MaxCPUBurstiness = 3.0
	p.CPUStrategy = "p95"
	e := mustEngine(t, p)
	ev := model.RestartEvidence{}
	// mean ~110, max 2000: burstiness ~18.
	vals := append(repeat(100, 119), 2000)
	cpu := series(model.ResourceCPU, 30*time.Second, vals...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(200e6, 120)...)
	rec := e.Recommend(workload(cpu, mem, 3000, 4e9, &ev))
	c := rec.Containers[0]
	if c.CPU.Decision != model.DecisionBlocked {
		t.Errorf("CPU decision = %s, want BLOCKED for burstiness %.1f (reason: %s)",
			c.CPU.Decision, c.CPU.Stats.Burstiness, c.CPU.Reason)
	}
}

// The invariant that makes the gate chain a safety mechanism rather than an
// additional source of risk:
//
//	result.Target >= min(proposed, current)
//
// A gate may hold at the current request, raise a target, or cancel a proposed
// change, but it may never produce something more aggressive than both the
// strategy's proposal and the deployed configuration.
//
// The bound is min() rather than the proposal alone because cancelling a marginal
// increase legitimately lowers the target back to the current request. An earlier
// version of this test asserted against the proposal alone and therefore passed
// while the engine's matching assertion panicked mid-experiment on a 9% proposed
// increase; the near-current increase cases below are the ones that were missing.
func TestGatesNeverGoBelowProposedOrCurrent(t *testing.T) {
	gates := []Gate{
		SufficiencyGate{MinSamples: 10, MinDuration: time.Minute, MinCoverage: 0.8},
		OOMProtectionGate{LookbackRelevance: 14 * 24 * time.Hour, RequireEvidence: true},
		UsageExceedsRequestGate{Percentile: "p99"},
		InstabilityGate{MaxBurstiness: 3},
		FloorGate{CPUFloorMilli: 10, MemoryFloorBytes: 32e6},
		MinChangeGate{MinRelativeChange: 0.1},
	}
	last := time.Now().Add(-time.Hour)
	var inputs []GateInput
	for _, kind := range []model.ResourceKind{model.ResourceCPU, model.ResourceMemory} {
		for _, cur := range []float64{1, 100, 1000, 2000, 1e9} {
			// Targets deliberately include values just above and just below the
			// current request, which is where the minimum-change gate acts and
			// where the naive form of this invariant breaks.
			for _, mult := range []float64{0.0005, 0.5, 0.91, 0.96, 1.0, 1.04, 1.09, 1.5, 3.0} {
				for _, evp := range []bool{true, false} {
					for _, ooms := range []int{0, 3} {
						inputs = append(inputs, GateInput{
							Resource: kind, Current: cur, Target: cur * mult,
							Summary: stats.Summary{
								Samples: 120, Mean: 100, P95: 200, P99: 400, Max: 2000,
								Burstiness: 20, WindowDuration: 2 * time.Hour,
							},
							Evidence:        model.RestartEvidence{OOMKills: ooms, LastOOM: &last},
							EvidencePresent: evp,
							Sufficiency:     model.DataSufficiency{Sufficient: true, Samples: 120},
						})
					}
				}
			}
		}
	}
	for _, g := range gates {
		for _, in := range inputs {
			res := g.Evaluate(in)
			if res.Block {
				// A blocking gate holds the line at the current value exactly.
				if res.Target != in.Current {
					t.Errorf("gate %s blocked but set target %v != current %v", g.ID(), res.Target, in.Current)
				}
				continue
			}
			floor := math.Min(in.Target, in.Current)
			if res.Target < floor-1e-9 {
				t.Errorf("gate %s violated the invariant: target %v < min(proposed %v, current %v) [resource %s]",
					g.ID(), res.Target, in.Target, in.Current, in.Resource)
			}
		}
	}
}

// The specific case that broke the naive invariant: a proposed increase too small
// to justify a rollout must be cancelled, returning the current request.
func TestMinChangeCancelsMarginalIncrease(t *testing.T) {
	g := MinChangeGate{MinRelativeChange: 0.10}
	res := g.Evaluate(GateInput{
		Resource: model.ResourceCPU, Current: 2000, Target: 2182,
		Sufficiency: model.DataSufficiency{Sufficient: true},
	})
	if !res.Fired {
		t.Error("a 9% increase should be suppressed")
	}
	if res.Target != 2000 {
		t.Errorf("target = %v, want the current request 2000", res.Target)
	}
	if res.Block {
		t.Error("a suppressed marginal change is NO_CHANGE, not BLOCKED: nothing was refused on safety grounds")
	}
	// And a change above the threshold must pass through untouched.
	res2 := g.Evaluate(GateInput{Resource: model.ResourceCPU, Current: 2000, Target: 2400})
	if res2.Fired || res2.Target != 2400 {
		t.Errorf("a 20%% increase should pass through, got %+v", res2)
	}
}

// End to end: a marginal proposed increase must surface as NO_CHANGE from the
// engine, not as an INCREASE nor as a panic.
func TestEngineReportsNoChangeForMarginalIncrease(t *testing.T) {
	p := sufficientPolicy()
	p.CPUStrategy = "max"
	p.CPUSafetyFactor = 1.0
	p.MinRelativeChange = 0.10
	e := mustEngine(t, p)
	ev := model.RestartEvidence{}
	// max usage 2090m against a 2000m request: a 4.5% proposed increase.
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(2090, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(200e6, 120)...)
	rec := e.Recommend(workload(cpu, mem, 2000, 4e9, &ev))
	c := rec.Containers[0]
	if c.CPU.Decision != model.DecisionNoChange {
		t.Errorf("CPU decision = %s, want NO_CHANGE (reason: %s)", c.CPU.Decision, c.CPU.Reason)
	}
	if c.CPU.Target != 2000 {
		t.Errorf("CPU target = %v, want the current 2000m", c.CPU.Target)
	}
	// The raw target must still record what the statistic proposed, so the
	// suppression is measurable.
	if c.CPU.RawTarget <= 2000 {
		t.Errorf("RawTarget = %v should record the proposed increase", c.CPU.RawTarget)
	}
}

// --- strategies -----------------------------------------------------------

func TestStrategyTargetsMatchTheirDefinition(t *testing.T) {
	s := stats.Summary{Mean: 100, P50: 90, P90: 180, P95: 200, P99: 400, Max: 1000}
	reg := NewRegistry(stats.LinearInterpolation)
	cases := []struct {
		id   string
		sf   float64
		want float64
	}{
		{"mean", 1.0, 100},
		{"mean", 1.5, 150},
		{"p50", 1.0, 90},
		{"p90", 1.0, 180},
		{"p95", 1.0, 200},
		{"p95", 1.15, 230},
		{"p99", 1.0, 400},
		{"max", 1.0, 1000},
		{"max", 1.25, 1250},
	}
	for _, c := range cases {
		st, err := reg.Get(c.id, c.sf)
		if err != nil {
			t.Fatalf("Get(%q): %v", c.id, err)
		}
		if got := st.Target(s); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s x%.2f = %v, want %v", c.id, c.sf, got, c.want)
		}
		if got := SafetyFactorOf(st); got != c.sf {
			t.Errorf("%s: SafetyFactorOf = %v, want %v", c.id, got, c.sf)
		}
		if st.Describe() == "" {
			t.Errorf("%s: Describe() must not be empty", c.id)
		}
	}
}

func TestRegistryRejectsUnknownStrategy(t *testing.T) {
	reg := NewRegistry(stats.LinearInterpolation)
	if _, err := reg.Get("p42", 1.0); err == nil {
		t.Error("expected an error for an unknown strategy")
	}
	ids := reg.IDs()
	if len(ids) < 7 {
		t.Errorf("registry should expose the full baseline set, got %v", ids)
	}
	// IDs must be in deterministic order: it fixes the row order of generated files.
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Errorf("registry IDs not sorted: %v", ids)
			break
		}
	}
}

// The "current" baseline must be a real code path through the engine, not a
// special case, so that its cost and risk are measured with identical machinery.
func TestCurrentBaselineLeavesRequestUnchanged(t *testing.T) {
	p := sufficientPolicy()
	p.CPUStrategy = "current"
	p.MemoryStrategy = "current"
	e := mustEngine(t, p)
	ev := model.RestartEvidence{}
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(50, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(100e6, 120)...)
	rec := e.Recommend(workload(cpu, mem, 2000, 4e9, &ev))
	c := rec.Containers[0]
	if c.CPU.Target != 2000 || c.Memory.Target != 4e9 {
		t.Errorf("current baseline changed the request: cpu %v, memory %v", c.CPU.Target, c.Memory.Target)
	}
	if c.CPU.Decision != model.DecisionNoChange || c.Memory.Decision != model.DecisionNoChange {
		t.Errorf("current baseline should be NO_CHANGE, got %s/%s", c.CPU.Decision, c.Memory.Decision)
	}
}

// --- risk classification --------------------------------------------------

func TestMemoryRiskIsHighBelowObservedMaximum(t *testing.T) {
	e := mustEngine(t, sufficientPolicy())
	ev := model.RestartEvidence{}
	summary := stats.Summary{Samples: 120, Mean: 500, P95: 800, P99: 900, Max: 1000, Burstiness: 2}
	if got := e.riskOf(model.ResourceMemory, 900, summary, ev, true); got != model.RiskHigh {
		t.Errorf("memory risk below observed max = %s, want HIGH", got)
	}
	if got := e.riskOf(model.ResourceMemory, 1050, summary, ev, true); got != model.RiskModerate {
		t.Errorf("memory risk at 1.05x observed max = %s, want MODERATE", got)
	}
	if got := e.riskOf(model.ResourceMemory, 1300, summary, ev, true); got != model.RiskLow {
		t.Errorf("memory risk at 1.3x observed max = %s, want LOW", got)
	}
	if got := e.riskOf(model.ResourceMemory, 1300, summary, ev, false); got != model.RiskUnknown {
		t.Errorf("memory risk without evidence = %s, want UNKNOWN", got)
	}
}

// CPU below the observed maximum is normal, not high risk: the asymmetry must be
// visible in the risk model too, or the engine would treat throttling as though
// it were a kill.
func TestCPURiskToleratesOperatingBelowMaximum(t *testing.T) {
	e := mustEngine(t, sufficientPolicy())
	ev := model.RestartEvidence{}
	summary := stats.Summary{Samples: 120, Mean: 500, P95: 800, P99: 900, Max: 1000, Burstiness: 2}
	if got := e.riskOf(model.ResourceCPU, 950, summary, ev, true); got != model.RiskLow {
		t.Errorf("CPU risk above p99 = %s, want LOW", got)
	}
	if got := e.riskOf(model.ResourceCPU, 850, summary, ev, true); got != model.RiskModerate {
		t.Errorf("CPU risk below p99 = %s, want MODERATE", got)
	}
	bursty := stats.Summary{Samples: 120, Mean: 100, P95: 200, P99: 400, Max: 3000, Burstiness: 30}
	if got := e.riskOf(model.ResourceCPU, 300, bursty, ev, true); got != model.RiskHigh {
		t.Errorf("CPU risk far below a bursty max = %s, want HIGH", got)
	}
	throttled := model.RestartEvidence{CPUThrottledRatio: 0.2}
	if got := e.riskOf(model.ResourceCPU, 950, summary, throttled, true); got != model.RiskModerate {
		t.Errorf("CPU risk with observed throttling = %s, want MODERATE", got)
	}
}

// --- observation window ---------------------------------------------------

// Changing the observation window must change what the engine sees, since RQ6 is
// implemented as a change to this one policy field.
func TestObservationWindowSelectsRecentData(t *testing.T) {
	// Old data is high, recent data is low. A short window must see only the low.
	vals := append(repeat(1000, 120), repeat(100, 120)...)
	cpu := series(model.ResourceCPU, 30*time.Second, vals...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(200e6, 240)...)
	ev := model.RestartEvidence{}

	short := sufficientPolicy()
	short.ObservationWindow = 30 * time.Minute // 60 samples
	short.CPUStrategy = "max"
	short.CPUSafetyFactor = 1.0
	eShort := mustEngine(t, short)

	long := short
	long.ObservationWindow = 4 * time.Hour
	eLong := mustEngine(t, long)

	rShort := eShort.Recommend(workload(cpu, mem, 3000, 4e9, &ev))
	rLong := eLong.Recommend(workload(cpu, mem, 3000, 4e9, &ev))

	if rShort.Containers[0].CPU.Target >= rLong.Containers[0].CPU.Target {
		t.Errorf("short-window target %v should be below long-window target %v",
			rShort.Containers[0].CPU.Target, rLong.Containers[0].CPU.Target)
	}
	if rShort.Containers[0].CPU.Stats.Samples >= rLong.Containers[0].CPU.Stats.Samples {
		t.Error("short window should summarise fewer samples")
	}
}

// --- rounding -------------------------------------------------------------

func TestRoundingNeverErodesTheSafetyMargin(t *testing.T) {
	p := sufficientPolicy()
	p.CPUStrategy = "max"
	p.CPUSafetyFactor = 1.10
	p.MemoryStrategy = "max"
	p.MemorySafetyFactor = 1.10
	p.MinRelativeChange = 0.0
	e := mustEngine(t, p)
	ev := model.RestartEvidence{}
	// Values chosen so the scaled target lands off the rounding grid.
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(333, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(700*float64(model.BytesPerMi)+7, 120)...)
	rec := e.Recommend(workload(cpu, mem, 2000, model.Bytes(4096*model.BytesPerMi), &ev))
	c := rec.Containers[0]
	if c.CPU.Target < c.CPU.RawTarget {
		t.Errorf("rounded CPU target %v is below the raw target %v", c.CPU.Target, c.CPU.RawTarget)
	}
	if c.Memory.Target < c.Memory.RawTarget {
		t.Errorf("rounded memory target %v is below the raw target %v", c.Memory.Target, c.Memory.RawTarget)
	}
}

func TestRoundingCanBeDisabledForAblation(t *testing.T) {
	p := sufficientPolicy()
	p.Rounding = false
	p.CPUStrategy = "max"
	p.CPUSafetyFactor = 1.0
	p.MinRelativeChange = 0.0
	e := mustEngine(t, p)
	ev := model.RestartEvidence{}
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(333.7, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(200e6, 120)...)
	rec := e.Recommend(workload(cpu, mem, 2000, 4e9, &ev))
	if got := rec.Containers[0].CPU.Target; math.Abs(got-333.7) > 1e-6 {
		t.Errorf("unrounded target = %v, want 333.7", got)
	}
}

// --- empty and degenerate input -------------------------------------------

func TestEmptySeriesIsInsufficientNotZero(t *testing.T) {
	e := mustEngine(t, DefaultPolicy())
	ev := model.RestartEvidence{}
	rec := e.Recommend(workload(
		model.Series{Resource: model.ResourceCPU, Step: 30 * time.Second},
		model.Series{Resource: model.ResourceMemory, Step: 30 * time.Second},
		2000, 4e9, &ev))
	c := rec.Containers[0]
	if c.CPU.Decision != model.DecisionInsufficientData {
		t.Errorf("empty CPU series: decision = %s, want INSUFFICIENT_DATA", c.CPU.Decision)
	}
	if c.CPU.Target != 0 {
		t.Errorf("empty series must not produce a target, got %v", c.CPU.Target)
	}
}

func TestZeroCurrentRequestProducesIncrease(t *testing.T) {
	// A container with no declared request is a real and common case.
	e := mustEngine(t, sufficientPolicy())
	ev := model.RestartEvidence{}
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(250, 120)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(300e6, 120)...)
	rec := e.Recommend(workload(cpu, mem, 0, 0, &ev))
	c := rec.Containers[0]
	if c.CPU.Decision != model.DecisionIncrease {
		t.Errorf("CPU decision = %s, want INCREASE from a zero request (reason: %s)", c.CPU.Decision, c.CPU.Reason)
	}
	if c.Memory.Decision != model.DecisionIncrease {
		t.Errorf("memory decision = %s, want INCREASE from a zero request", c.Memory.Decision)
	}
}
