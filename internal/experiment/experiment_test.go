package experiment

import (
	"context"
	"encoding/csv"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/simulator"
	"github.com/anishc23/k8s-cost-optimizer/pkg/humanize"
)

// hd and hds shorten the humanize conversions in test fixtures.
func hd(d time.Duration) humanize.Duration { return humanize.Duration(d) }

func hds(ds ...time.Duration) humanize.Durations {
	out := make(humanize.Durations, len(ds))
	for i, d := range ds {
		out[i] = humanize.Duration(d)
	}
	return out
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func smallConfig() Config {
	return Config{
		Name:               "unit-test",
		Workloads:          []simulator.Class{simulator.ClassStableCPU, simulator.ClassBurstyCPU},
		CPUStrategies:      []string{"p95", "max"},
		MemoryStrategies:   []string{"max"},
		ObservationWindows: hds(6 * time.Hour),
		SafetyFactors:      []float64{1.0, 1.2},
		Seeds:              2,
		BaseSeed:           7,
		TraceDuration:      hd(12 * time.Hour),
		Step:               hd(time.Minute),
		EvaluationHorizon:  hd(2 * time.Hour),
		Feasibility:        DefaultFeasibility(),
	}
}

// --- configuration validation --------------------------------------------

func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Config)
		valid bool
	}{
		{"baseline", func(*Config) {}, true},
		{"no name", func(c *Config) { c.Name = "" }, false},
		{"no cpu strategies", func(c *Config) { c.CPUStrategies = nil }, false},
		{"no memory strategies", func(c *Config) { c.MemoryStrategies = nil }, false},
		{"no windows", func(c *Config) { c.ObservationWindows = nil }, false},
		{"no safety factors", func(c *Config) { c.SafetyFactors = nil }, false},
		{"safety below 1", func(c *Config) { c.SafetyFactors = []float64{0.8} }, false},
		{"zero seeds", func(c *Config) { c.Seeds = 0 }, false},
		{"zero step", func(c *Config) { c.Step = 0 }, false},
		{"zero horizon", func(c *Config) { c.EvaluationHorizon = 0 }, false},
		{"negative window", func(c *Config) { c.ObservationWindows = hds(-time.Hour) }, false},
	}
	for _, c := range cases {
		cfg := smallConfig()
		c.mut(&cfg)
		err := cfg.Validate()
		if c.valid != (err == nil) {
			t.Errorf("%s: valid=%v but err=%v", c.name, c.valid, err)
		}
	}
}

// The trace must be long enough to hold the longest window plus the held-out
// horizon; otherwise the longest-window condition is silently fitted on
// truncated data and the observation-window comparison is invalid.
func TestConfigRejectsTraceTooShortForWindowPlusHorizon(t *testing.T) {
	cfg := smallConfig()
	cfg.ObservationWindows = hds(24 * time.Hour)
	cfg.EvaluationHorizon = hd(4 * time.Hour)
	cfg.TraceDuration = hd(24 * time.Hour) // needs 28h
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected rejection when the trace cannot hold window + horizon")
	}
	t.Logf("correctly rejected: %v", err)
}

func TestConditionCountMatchesMatrix(t *testing.T) {
	cfg := smallConfig()
	// 2 classes x 2 cpu strategies x 1 memory strategy x 1 window x 2 factors x 2 seeds
	if got, want := cfg.Conditions(), 2*2*1*1*2*2; got != want {
		t.Errorf("Conditions() = %d, want %d", got, want)
	}
	r, err := NewRunner(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(r.expand()); got != cfg.Conditions() {
		t.Errorf("expand() produced %d conditions, Conditions() predicted %d", got, cfg.Conditions())
	}
}

func TestUnifiedStrategyCollapsesMemoryDimension(t *testing.T) {
	cfg := smallConfig()
	cfg.MemoryStrategies = []string{"max", "p99", "p95"}
	cfg.UnifiedStrategy = true
	r, err := NewRunner(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	conds := r.expand()
	if len(conds) != cfg.Conditions() {
		t.Errorf("expand() = %d, Conditions() = %d", len(conds), cfg.Conditions())
	}
	for _, c := range conds {
		if c.cpuStrat != c.memStrat {
			t.Errorf("unified strategy should force memory=cpu, got %s/%s", c.cpuStrat, c.memStrat)
		}
	}
}

// --- run semantics --------------------------------------------------------

func TestRunProducesOneRecordPerCondition(t *testing.T) {
	cfg := smallConfig()
	r, err := NewRunner(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != cfg.Conditions() {
		t.Errorf("got %d records, want %d conditions", len(res.Records), cfg.Conditions())
	}
	if res.Provenance.Records != len(res.Records) {
		t.Error("provenance record count must match the data")
	}
	if res.Provenance.ConfigHash == "" {
		t.Error("provenance must carry a config hash")
	}
	for i, rec := range res.Records {
		if rec.WorkloadClass == "" || rec.CPUStrategy == "" {
			t.Errorf("record %d is not fully populated: %+v", i, rec)
		}
		if rec.CurrentMonthlyUSD <= 0 {
			t.Errorf("record %d has no current cost", i)
		}
	}
}

// Results must be reproducible from the configuration alone.
func TestRunIsReproducible(t *testing.T) {
	cfg := smallConfig()
	run := func() []Record {
		r, err := NewRunner(cfg, quietLogger())
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return res.Records
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("record counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].RecommendedCPUMilli != b[i].RecommendedCPUMilli ||
			a[i].RecommendedMemoryBytes != b[i].RecommendedMemoryBytes ||
			a[i].SavingsFraction != b[i].SavingsFraction ||
			a[i].OOMKills != b[i].OOMKills {
			t.Fatalf("record %d differs between runs:\n a=%+v\n b=%+v", i, a[i], b[i])
		}
	}
}

// Parallelism must not change results: conditions are independent by
// construction, and if they were not, every reported number would depend on the
// machine it was computed on.
func TestParallelismDoesNotChangeResults(t *testing.T) {
	cfg := smallConfig()
	run := func(concurrency int) []Record {
		r, err := NewRunner(cfg, quietLogger())
		if err != nil {
			t.Fatal(err)
		}
		r.Concurrency = concurrency
		res, err := r.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return res.Records
	}
	serial, parallel := run(1), run(8)
	for i := range serial {
		if serial[i] != parallel[i] {
			t.Fatalf("record %d differs between serial and parallel runs", i)
		}
	}
}

// The engine must be scored on data it never saw. This test confirms the
// fitting window and the evaluation horizon do not overlap, which is the
// property that makes the evaluation a prediction task rather than a
// description of the fitting data.
func TestFitAndEvaluationWindowsDoNotOverlap(t *testing.T) {
	spec, err := simulator.SpecByClass(simulator.ClassStableCPU, 12*time.Hour, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	full, err := simulator.Generate(spec)
	if err != nil {
		t.Fatal(err)
	}
	fit, eval, err := splitTrace(full, 6*time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fitEnd := fit.CPUDemand.Samples[fit.CPUDemand.Len()-1].Timestamp
	evalStart := eval.CPUDemand.Samples[0].Timestamp
	if !evalStart.After(fitEnd) {
		t.Errorf("evaluation horizon starts at %v, which is not after the fitting window end %v",
			evalStart, fitEnd)
	}
	if got := eval.Spec.Duration; got != 2*time.Hour {
		t.Errorf("horizon duration = %v, want 2h", got)
	}
	// The horizon must be anchored at the end of the trace, so that every window
	// length is scored on the same future.
	traceEnd := full.CPUDemand.Samples[full.CPUDemand.Len()-1].Timestamp
	evalEnd := eval.CPUDemand.Samples[eval.CPUDemand.Len()-1].Timestamp
	if !evalEnd.Equal(traceEnd) {
		t.Errorf("horizon ends at %v, not at the trace end %v", evalEnd, traceEnd)
	}
}

// Different window lengths must be scored against the same held-out period,
// otherwise RQ6 would be comparing different futures rather than different
// histories.
func TestAllWindowsShareTheSameEvaluationHorizon(t *testing.T) {
	spec, _ := simulator.SpecByClass(simulator.ClassStableCPU, 24*time.Hour, time.Minute, 1)
	full, _ := simulator.Generate(spec)
	var firsts []time.Time
	for _, w := range []time.Duration{time.Hour, 6 * time.Hour, 12 * time.Hour} {
		_, eval, err := splitTrace(full, w, 4*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		firsts = append(firsts, eval.CPUDemand.Samples[0].Timestamp)
	}
	for i := 1; i < len(firsts); i++ {
		if !firsts[i].Equal(firsts[0]) {
			t.Errorf("window %d is scored on a different horizon: %v vs %v", i, firsts[i], firsts[0])
		}
	}
}

// Ground truth for a sub-trace must be recomputed for that sub-trace, not
// inherited from the whole trace: scoring a two-hour horizon against a seven-day
// maximum would make every recommendation look under-provisioned.
func TestSubTraceRecomputesGroundTruth(t *testing.T) {
	// A trace whose demand is much higher in its first half.
	spec, _ := simulator.SpecByClass(simulator.ClassPeriodicCPU, 24*time.Hour, time.Minute, 3)
	full, _ := simulator.Generate(spec)
	_, eval, err := splitTrace(full, 12*time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if eval.GroundTruth.CPUMaxMilli == full.GroundTruth.CPUMaxMilli &&
		eval.GroundTruth.CPUMeanMilli == full.GroundTruth.CPUMeanMilli {
		t.Error("sub-trace ground truth appears to be inherited from the full trace")
	}
	// It must be internally consistent with its own samples.
	var m float64
	for _, s := range eval.CPUDemand.Samples {
		if s.Value > m {
			m = s.Value
		}
	}
	if math.Abs(eval.GroundTruth.CPUMaxMilli-m) > 1e-9 {
		t.Errorf("sub-trace ground truth max %v does not match its samples %v",
			eval.GroundTruth.CPUMaxMilli, m)
	}
}

// Ablation flags must actually change engine behaviour, or an ablation study
// would report that a component does not matter merely because it was never
// disabled.
func TestAblationFlagsReachThePolicy(t *testing.T) {
	cfg := smallConfig()
	cfg.DisableOOMProtection = true
	cfg.DisableFloors = true
	cfg.DisableMinChange = true
	cfg.DisableRounding = true
	cfg.DisableUsageExceeds = true
	r, err := NewRunner(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	p := r.policyFor(condition{cpuStrat: "p95", memStrat: "max", cpuSF: 1.0, memSF: 1.0, window: time.Hour})
	if p.OOMProtection {
		t.Error("DisableOOMProtection did not reach the policy")
	}
	if p.CPUFloorMilli != 0 || p.MemoryFloorBytes != 0 {
		t.Error("DisableFloors did not reach the policy")
	}
	if p.MinRelativeChange != 0 {
		t.Error("DisableMinChange did not reach the policy")
	}
	if p.Rounding {
		t.Error("DisableRounding did not reach the policy")
	}
	if p.UsageExceedsRequestPercentile != "" {
		t.Error("DisableUsageExceeds did not reach the policy")
	}
	// And the record must carry the flags, so ablation and baseline rows can be
	// concatenated and separated by filtering.
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].OOMProtectionEnabled {
		t.Error("record should mark OOM protection as disabled")
	}
}

// --- metrics --------------------------------------------------------------

func TestFeasibilityConstraintIsAsymmetric(t *testing.T) {
	c := DefaultFeasibility()
	// A single OOMKill is infeasible, however small.
	if c.Satisfied(simulator.Outcome{OOMKills: 1}) {
		t.Error("one OOMKill must be infeasible")
	}
	// Small CPU throttling is feasible: the asymmetry is the point.
	if !c.Satisfied(simulator.Outcome{CPUThrottleFraction: 0.005}) {
		t.Error("0.5%% CPU throttling should be feasible")
	}
	if c.Satisfied(simulator.Outcome{CPUThrottleFraction: 0.05}) {
		t.Error("5%% CPU throttling should be infeasible under the default constraint")
	}
}

func TestFeasibleSavingsIsZeroWhenInfeasible(t *testing.T) {
	tr := simulator.Trace{GroundTruth: simulator.GroundTruth{CPUP99Milli: 100, MemoryMaxBytes: 1000}}
	out := simulator.Outcome{OOMKills: 2, CPURequestMilli: 50, MemoryRequestBytes: 500}
	m := ComputeMetrics(tr, out, 100, 40, DefaultFeasibility())
	if m.SavingsFraction <= 0 {
		t.Error("the raw savings fraction should still be reported")
	}
	if m.FeasibleSavings != 0 {
		t.Errorf("FeasibleSavings = %v, want 0 for an infeasible outcome", m.FeasibleSavings)
	}
	if m.Feasible {
		t.Error("Feasible should be false")
	}
	if m.SurvivedWithoutOOM {
		t.Error("SurvivedWithoutOOM should be false with 2 OOMKills")
	}
}

func TestRelativeErrorsUseTheRightReference(t *testing.T) {
	tr := simulator.Trace{GroundTruth: simulator.GroundTruth{
		CPUP99Milli: 100, CPUMaxMilli: 500,
		MemoryMaxBytes: 1000, MemoryP99Bytes: 800,
	}}
	out := simulator.Outcome{CPURequestMilli: 120, MemoryRequestBytes: 1200}
	m := ComputeMetrics(tr, out, 100, 80, DefaultFeasibility())
	// CPU error is measured against true p99 (a realistic operating target).
	if math.Abs(m.CPURelativeError-0.2) > 1e-9 {
		t.Errorf("CPURelativeError = %v, want 0.2 (vs p99)", m.CPURelativeError)
	}
	// Memory error is measured against true max, because anything below it kills.
	if math.Abs(m.MemoryRelativeError-0.2) > 1e-9 {
		t.Errorf("MemoryRelativeError = %v, want 0.2 (vs max)", m.MemoryRelativeError)
	}
}

// --- stability ------------------------------------------------------------

func TestStabilityLogRatioIsSymmetric(t *testing.T) {
	// A doubling and a halving must register as the same magnitude of change;
	// a percentage difference would report +100% and -50%.
	doubling := ComputeStability([]float64{100, 200}, 0.1)
	halving := ComputeStability([]float64{200, 100}, 0.1)
	if math.Abs(doubling.MedianAbsLog2Ratio-halving.MedianAbsLog2Ratio) > 1e-12 {
		t.Errorf("asymmetric: doubling %v vs halving %v",
			doubling.MedianAbsLog2Ratio, halving.MedianAbsLog2Ratio)
	}
	if math.Abs(doubling.MedianAbsLog2Ratio-1.0) > 1e-12 {
		t.Errorf("a doubling should score 1.0, got %v", doubling.MedianAbsLog2Ratio)
	}
}

func TestStabilityOfConstantRecommendationIsZero(t *testing.T) {
	s := ComputeStability([]float64{500, 500, 500, 500}, 0.1)
	if s.MedianAbsLog2Ratio != 0 || s.MaxAbsLog2Ratio != 0 {
		t.Errorf("a constant recommendation must be perfectly stable, got %+v", s)
	}
	if s.ChangeRate != 0 {
		t.Errorf("ChangeRate = %v, want 0", s.ChangeRate)
	}
	if s.RangeRatio != 1 {
		t.Errorf("RangeRatio = %v, want 1", s.RangeRatio)
	}
	if s.Recomputations != 3 {
		t.Errorf("Recomputations = %d, want 3", s.Recomputations)
	}
}

// A zero is an absent recommendation (INSUFFICIENT_DATA), not a recommendation
// of nothing. Treating it as a swing to zero would report enormous volatility
// for a recommender that simply declined to answer.
func TestStabilitySkipsAbsentRecommendations(t *testing.T) {
	s := ComputeStability([]float64{500, 0, 500}, 0.1)
	if s.MedianAbsLog2Ratio != 0 {
		t.Errorf("absent recommendations must be skipped, got volatility %v", s.MedianAbsLog2Ratio)
	}
	if s.Recomputations != 1 {
		t.Errorf("Recomputations = %d, want 1 after skipping the gap", s.Recomputations)
	}
	// Too few usable values yields an empty result rather than a bogus zero.
	if got := ComputeStability([]float64{0, 0}, 0.1); got.Recomputations != 0 {
		t.Errorf("no usable values should give zero recomputations, got %+v", got)
	}
}

func TestStabilityChangeRateUsesThreshold(t *testing.T) {
	// 100 -> 104 is 4%; below a 10% threshold it is not an operational change.
	s := ComputeStability([]float64{100, 104, 108}, 0.10)
	if s.ChangeRate != 0 {
		t.Errorf("ChangeRate = %v, want 0 for sub-threshold moves", s.ChangeRate)
	}
	s2 := ComputeStability([]float64{100, 150, 220}, 0.10)
	if s2.ChangeRate != 1 {
		t.Errorf("ChangeRate = %v, want 1 for large moves", s2.ChangeRate)
	}
}

func TestStabilityIsMeasuredWhenConfigured(t *testing.T) {
	cfg := smallConfig()
	cfg.TraceDuration = hd(24 * time.Hour)
	cfg.ObservationWindows = hds(6 * time.Hour)
	cfg.EvaluationHorizon = hd(2 * time.Hour)
	cfg.StabilityRecomputations = 5
	cfg.Workloads = []simulator.Class{simulator.ClassBurstyCPU}
	cfg.CPUStrategies = []string{"p95"}
	cfg.Seeds = 1
	cfg.SafetyFactors = []float64{1.0}
	r, err := NewRunner(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rec := res.Records[0]
	if rec.Stability == nil {
		t.Fatal("stability metrics were requested but not recorded")
	}
	if rec.Stability.Recomputations < 2 {
		t.Errorf("expected several recomputations, got %d", rec.Stability.Recomputations)
	}
}

// --- output ---------------------------------------------------------------

func TestCSVRoundTripAndSchema(t *testing.T) {
	cfg := smallConfig()
	r, _ := NewRunner(cfg, quietLogger())
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "results.csv")
	jsonPath := filepath.Join(dir, "results.json")
	if err := WriteCSV(res, csvPath); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(res, jsonPath); err != nil {
		t.Fatal(err)
	}
	if err := WriteProvenance(res, filepath.Join(dir, "provenance.json")); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(res.Records)+1 {
		t.Errorf("CSV has %d rows, want %d records plus a header", len(rows), len(res.Records))
	}
	if len(rows[0]) != len(csvColumns) {
		t.Errorf("header has %d columns, want %d", len(rows[0]), len(csvColumns))
	}
	// Every data row must have exactly as many fields as the header: a mismatch
	// would silently shift columns in the analysis.
	for i, row := range rows[1:] {
		if len(row) != len(csvColumns) {
			t.Fatalf("row %d has %d fields, want %d", i, len(row), len(csvColumns))
		}
	}
	// And the declared column list must match the row builder.
	if got := len(res.Records[0].csvRow()); got != len(csvColumns) {
		t.Errorf("csvRow() produces %d fields but csvColumns declares %d", got, len(csvColumns))
	}
}

func TestConfigHashChangesWithConfig(t *testing.T) {
	a := smallConfig()
	b := smallConfig()
	if ConfigHash(a) != ConfigHash(b) {
		t.Error("identical configs must hash identically")
	}
	b.SafetyFactors = []float64{1.0, 1.5}
	if ConfigHash(a) == ConfigHash(b) {
		t.Error("differing configs must hash differently")
	}
}

// --- end-to-end sanity ----------------------------------------------------

// A generous strategy on a stable workload must be feasible and must save
// money. If this failed, the whole pipeline would be suspect regardless of what
// the comparative results said.
func TestGenerousStrategyOnStableWorkloadIsFeasibleAndSaves(t *testing.T) {
	cfg := smallConfig()
	cfg.Workloads = []simulator.Class{simulator.ClassStableCPU}
	cfg.CPUStrategies = []string{"max"}
	cfg.MemoryStrategies = []string{"max"}
	cfg.SafetyFactors = []float64{1.5}
	cfg.Seeds = 3
	r, _ := NewRunner(cfg, quietLogger())
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range res.Records {
		if !rec.Feasible {
			t.Errorf("max x1.5 on a stable workload should be feasible: oom=%d throttle=%.4f",
				rec.OOMKills, rec.CPUThrottleFraction)
		}
		if rec.SavingsFraction <= 0 {
			t.Errorf("a 5x over-provisioned stable workload should yield savings, got %.3f",
				rec.SavingsFraction)
		}
	}
}
