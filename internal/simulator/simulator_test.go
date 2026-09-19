package simulator

import (
	"math"
	"testing"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/internal/stats"
)

const (
	testDuration = 24 * time.Hour
	testStep     = 30 * time.Second
	testSeed     = 42
)

// Determinism is the foundation of every reproducibility claim in the project:
// if the same spec produced different traces, no result could be re-derived.
func TestGenerateIsDeterministic(t *testing.T) {
	spec, err := SpecByClass(ClassBurstyCPU, testDuration, testStep, testSeed)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Generate(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(spec)
	if err != nil {
		t.Fatal(err)
	}
	if a.CPUDemand.Len() != b.CPUDemand.Len() {
		t.Fatalf("lengths differ: %d vs %d", a.CPUDemand.Len(), b.CPUDemand.Len())
	}
	for i := range a.CPUDemand.Samples {
		if a.CPUDemand.Samples[i].Value != b.CPUDemand.Samples[i].Value {
			t.Fatalf("CPU sample %d differs: %v vs %v", i,
				a.CPUDemand.Samples[i].Value, b.CPUDemand.Samples[i].Value)
		}
		if a.MemoryDemand.Samples[i].Value != b.MemoryDemand.Samples[i].Value {
			t.Fatalf("memory sample %d differs", i)
		}
	}
	if a.GroundTruth != b.GroundTruth {
		t.Errorf("ground truth differs: %+v vs %+v", a.GroundTruth, b.GroundTruth)
	}
}

// Per-class seed derivation must isolate classes: adding a class to the catalog
// must not change the traces of the others, or every previously published result
// would silently change.
func TestSeedsAreIndependentAcrossClasses(t *testing.T) {
	specs := Catalog(testDuration, testStep, testSeed)
	seen := map[int64]string{}
	for _, s := range specs {
		if prev, dup := seen[s.Seed]; dup {
			t.Errorf("seed %d shared by %s and %s", s.Seed, prev, s.Name)
		}
		seen[s.Seed] = s.Name
	}
}

func TestGenerateRejectsInvalidSpecs(t *testing.T) {
	cases := []Spec{
		{Name: "no-duration", Step: time.Second},
		{Name: "no-step", Duration: time.Hour},
		{Name: "step-too-big", Duration: time.Minute, Step: time.Hour},
		{Name: "negative-cpu", Duration: time.Hour, Step: time.Minute, BaselineCPU: -1},
		{Name: "negative-mem", Duration: time.Hour, Step: time.Minute, BaselineMemory: -1},
	}
	for _, c := range cases {
		if _, err := Generate(c); err == nil {
			t.Errorf("spec %q should have been rejected", c.Name)
		}
	}
}

func TestDemandIsNonNegative(t *testing.T) {
	traces, err := GenerateCatalog(testDuration, testStep, testSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range traces {
		for i, s := range tr.CPUDemand.Samples {
			if s.Value < 0 {
				t.Errorf("%s: negative CPU demand at %d: %v", tr.Spec.Name, i, s.Value)
			}
		}
		for i, s := range tr.MemoryDemand.Samples {
			if s.Value < 0 {
				t.Errorf("%s: negative memory demand at %d: %v", tr.Spec.Name, i, s.Value)
			}
		}
	}
}

// The ground-truth percentile implementation is deliberately separate from
// internal/stats so that an estimator bug cannot cancel out between the
// recommendation and the reference it is scored against. This test confirms the
// two agree where they should, which is what makes that separation safe rather
// than merely redundant.
func TestGroundTruthPercentilesAgreeWithStatsNearestRank(t *testing.T) {
	traces, err := GenerateCatalog(testDuration, testStep, testSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range traces {
		vals := tr.CPUDemand.Values()
		for _, c := range []struct {
			p    float64
			got  float64
			name string
		}{
			{0.95, tr.GroundTruth.CPUP95Milli, "p95"},
			{0.99, tr.GroundTruth.CPUP99Milli, "p99"},
		} {
			want := stats.Percentile(vals, c.p, stats.NearestRank)
			if math.Abs(c.got-want) > 1e-9 {
				t.Errorf("%s %s: simulator %v vs stats %v", tr.Spec.Name, c.name, c.got, want)
			}
		}
		if want := stats.Max(vals); math.Abs(tr.GroundTruth.CPUMaxMilli-want) > 1e-9 {
			t.Errorf("%s max: simulator %v vs stats %v", tr.Spec.Name, tr.GroundTruth.CPUMaxMilli, want)
		}
	}
}

// Each class must actually exhibit the behaviour its name claims. Without this,
// a results table broken down "by workload class" would be reporting on labels
// rather than on behaviour.
func TestClassesExhibitTheirDefiningBehaviour(t *testing.T) {
	traces, err := GenerateCatalog(7*24*time.Hour, testStep, testSeed)
	if err != nil {
		t.Fatal(err)
	}
	byClass := map[Class]Trace{}
	for _, tr := range traces {
		byClass[tr.Spec.Class] = tr
	}

	stable := byClass[ClassStableCPU]
	if b := stable.GroundTruth.CPUMaxMilli / stable.GroundTruth.CPUMeanMilli; b > 2.0 {
		t.Errorf("stable-cpu burstiness %.2f should be low (<2)", b)
	}

	bursty := byClass[ClassBurstyCPU]
	if b := bursty.GroundTruth.CPUMaxMilli / bursty.GroundTruth.CPUMeanMilli; b < 3.0 {
		t.Errorf("bursty-cpu burstiness %.2f should be high (>3)", b)
	}

	// The spiky class must place its peak above p99, otherwise it does not
	// isolate the failure mode it was designed to isolate: a workload where
	// every percentile policy short of the maximum under-provisions.
	spiky := byClass[ClassSpikyCPU]
	if spiky.GroundTruth.CPUP99Milli >= spiky.GroundTruth.CPUMaxMilli*0.2 {
		t.Errorf("spiky-cpu p99 (%.0f) should sit far below max (%.0f): spikes must be rarer than 1%% of samples",
			spiky.GroundTruth.CPUP99Milli, spiky.GroundTruth.CPUMaxMilli)
	}

	// bursty-cpu must be the intermediate case: p95 misses the bursts while p99
	// captures them. If this inverted, the three CPU classes would no longer
	// span three distinct regimes and RQ2's ranking would be untestable.
	if bursty.GroundTruth.CPUP95Milli >= bursty.GroundTruth.CPUMaxMilli*0.2 {
		t.Errorf("bursty-cpu p95 (%.0f) should miss the bursts (max %.0f)",
			bursty.GroundTruth.CPUP95Milli, bursty.GroundTruth.CPUMaxMilli)
	}
	if bursty.GroundTruth.CPUP99Milli <= bursty.GroundTruth.CPUMaxMilli*0.5 {
		t.Errorf("bursty-cpu p99 (%.0f) should capture the bursts (max %.0f)",
			bursty.GroundTruth.CPUP99Milli, bursty.GroundTruth.CPUMaxMilli)
	}

	// bursty-memory is the sharpest test of the CPU/memory asymmetry: p95 must
	// miss the transient allocation that p99 catches, because on memory that
	// miss is fatal rather than merely slow.
	bmem := byClass[ClassBurstyMemory]
	if bmem.GroundTruth.MemoryP95Bytes >= bmem.GroundTruth.MemoryMaxBytes*0.5 {
		t.Errorf("bursty-memory p95 (%.0f) should miss the allocation peak (max %.0f)",
			bmem.GroundTruth.MemoryP95Bytes, bmem.GroundTruth.MemoryMaxBytes)
	}
	if bmem.GroundTruth.MemoryP99Bytes <= bmem.GroundTruth.MemoryMaxBytes*0.5 {
		t.Errorf("bursty-memory p99 (%.0f) should capture the allocation peak (max %.0f)",
			bmem.GroundTruth.MemoryP99Bytes, bmem.GroundTruth.MemoryMaxBytes)
	}

	// Periodic must spend a large share of samples near its peak, unlike bursty.
	// This is the structural difference the workload-class hypothesis rests on.
	periodic := byClass[ClassPeriodicCPU]
	if periodic.GroundTruth.CPUP95Milli <= periodic.GroundTruth.CPUMaxMilli*0.5 {
		t.Errorf("periodic-cpu p95 (%.0f) should be close to max (%.0f)",
			periodic.GroundTruth.CPUP95Milli, periodic.GroundTruth.CPUMaxMilli)
	}
	if bursty.GroundTruth.CPUP95Milli >= periodic.GroundTruth.CPUP95Milli {
		t.Errorf("bursty-cpu p95 (%.0f) should sit far below periodic-cpu p95 (%.0f): the two classes must differ in peak *duration*, not only peak height",
			bursty.GroundTruth.CPUP95Milli, periodic.GroundTruth.CPUP95Milli)
	}

	growing := byClass[ClassGrowingMemory]
	if r := growing.GroundTruth.MemoryMaxBytes / growing.GroundTruth.MemoryMeanBytes; r < 1.3 {
		t.Errorf("growing-memory max/mean %.2f should show clear growth (>1.3)", r)
	}

	idle := byClass[ClassIdle]
	if idle.GroundTruth.CPUMeanMilli > 20 {
		t.Errorf("idle mean CPU %.1fm should be tiny", idle.GroundTruth.CPUMeanMilli)
	}
}

// Every class must start over-provisioned, or there is no waste for the engine
// to find and RQ1 is unanswerable.
func TestCatalogIsOverProvisioned(t *testing.T) {
	traces, err := GenerateCatalog(testDuration, testStep, testSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range traces {
		if tr.Spec.DeclaredCPU <= tr.GroundTruth.CPUMaxMilli {
			t.Errorf("%s: declared CPU %.0fm does not exceed max demand %.0fm",
				tr.Spec.Name, tr.Spec.DeclaredCPU, tr.GroundTruth.CPUMaxMilli)
		}
		if tr.Spec.DeclaredMemory <= tr.GroundTruth.MemoryMaxBytes {
			t.Errorf("%s: declared memory %.0f does not exceed max demand %.0f",
				tr.Spec.Name, tr.Spec.DeclaredMemory, tr.GroundTruth.MemoryMaxBytes)
		}
	}
}

// --- Replay ---------------------------------------------------------------

func TestReplayGenerousConfigHasNoViolations(t *testing.T) {
	tr := mustTrace(t, ClassBurstyCPU)
	out := Replay(tr, tr.GroundTruth.CPUMaxMilli*1.01, tr.GroundTruth.MemoryMaxBytes*1.01, DefaultConfig())
	if out.CPUViolationRate != 0 {
		t.Errorf("CPU violation rate %.4f should be 0 above max demand", out.CPUViolationRate)
	}
	if out.OOMKills != 0 {
		t.Errorf("OOMKills %d should be 0 above max demand", out.OOMKills)
	}
	if out.CPUThrottledCoreSeconds != 0 {
		t.Errorf("throttled core-seconds %.4f should be 0", out.CPUThrottledCoreSeconds)
	}
}

func TestReplayTightConfigProducesViolations(t *testing.T) {
	tr := mustTrace(t, ClassBurstyCPU)
	// Size to the mean: for a bursty workload this must fail often.
	out := Replay(tr, tr.GroundTruth.CPUMeanMilli, tr.GroundTruth.MemoryMeanBytes, DefaultConfig())
	if out.CPUViolationRate <= 0 {
		t.Error("sizing to mean CPU on a bursty workload should violate")
	}
	if out.CPUThrottleFraction <= 0 {
		t.Error("expected non-zero throttle fraction")
	}
	if out.OOMKills == 0 {
		t.Error("sizing memory to the mean should produce OOMKills")
	}
	if out.TimeToFirstOOM == nil {
		t.Error("expected a time to first OOM")
	}
}

// Violation rate and throttle fraction must measure different things: frequency
// versus magnitude. A synthetic check that they can diverge protects the
// interpretation of the trade-off curves.
func TestViolationRateAndThrottleFractionAreDistinct(t *testing.T) {
	step := time.Minute
	// One sample massively short, many samples slightly short.
	mkTrace := func(vals []float64) Trace {
		samples := make([]model.Sample, len(vals))
		base := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
		for i, v := range vals {
			samples[i] = model.Sample{Timestamp: base.Add(time.Duration(i) * step), Value: v}
		}
		tr := Trace{
			Spec:      Spec{Name: "synthetic", Duration: time.Duration(len(vals)-1) * step, Step: step},
			CPUDemand: model.Series{Resource: model.ResourceCPU, Samples: samples, Step: step},
		}
		tr.GroundTruth = ComputeGroundTruth(tr)
		return tr
	}
	// Case A: 10 samples each 10% over the request.
	a := mkTrace([]float64{110, 110, 110, 110, 110, 110, 110, 110, 110, 110})
	oa := Replay(a, 100, 0, DefaultConfig())
	// Case B: 1 sample 1000% over, 9 at the request.
	b := mkTrace([]float64{100, 100, 100, 100, 100, 100, 100, 100, 100, 1100})
	ob := Replay(b, 100, 0, DefaultConfig())

	if oa.CPUViolationRate <= ob.CPUViolationRate {
		t.Errorf("case A should violate more often: %.2f vs %.2f", oa.CPUViolationRate, ob.CPUViolationRate)
	}
	if ob.CPUThrottleFraction <= oa.CPUThrottleFraction {
		t.Errorf("case B should throttle more work: %.4f vs %.4f", ob.CPUThrottleFraction, oa.CPUThrottleFraction)
	}
	if ob.CPUMaxShortfallMilli <= oa.CPUMaxShortfallMilli {
		t.Error("case B should have the larger worst-moment shortfall")
	}
}

// The OOM cooldown must make the kill count measure distinct failure episodes,
// not excursion length.
func TestOOMCooldownCountsEpisodesNotSamples(t *testing.T) {
	step := 30 * time.Second
	base := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	// 20 consecutive samples (10 minutes) above the limit: one sustained episode.
	n := 20
	samples := make([]model.Sample, n)
	for i := 0; i < n; i++ {
		samples[i] = model.Sample{Timestamp: base.Add(time.Duration(i) * step), Value: 200}
	}
	tr := Trace{
		Spec:         Spec{Name: "sustained", Duration: time.Duration(n-1) * step, Step: step},
		MemoryDemand: model.Series{Resource: model.ResourceMemory, Samples: samples, Step: step},
	}
	tr.GroundTruth = ComputeGroundTruth(tr)

	cfg := DefaultConfig() // 2 minute cooldown
	out := Replay(tr, 0, 100, cfg)
	if out.MemoryViolationRate != 1.0 {
		t.Errorf("violation rate should be 1.0, got %.2f", out.MemoryViolationRate)
	}
	// 10 minutes of excursion with a 2 minute cooldown: 5-6 episodes, not 20.
	if out.OOMKills >= n {
		t.Errorf("OOMKills %d should be far fewer than the %d violating samples", out.OOMKills, n)
	}
	if out.OOMKills < 2 {
		t.Errorf("OOMKills %d should count several episodes over 10 minutes", out.OOMKills)
	}
}

// Censoring is the mechanism that makes the evaluation honest; if observation
// were not censored, a tight configuration would be scored against evidence it
// had suppressed.
func TestObservedSeriesIsCensoredByConfiguration(t *testing.T) {
	tr := mustTrace(t, ClassBurstyCPU)
	cap := tr.GroundTruth.CPUMeanMilli
	cpu, _ := ObservedSeries(tr, cap, 0, DefaultConfig())
	for i, s := range cpu.Samples {
		if s.Value > cap+1e-9 {
			t.Fatalf("observed CPU sample %d = %v exceeds the ceiling %v", i, s.Value, cap)
		}
	}
	// And demand itself must be untouched: replay must never mutate the trace.
	if stats.Max(tr.CPUDemand.Values()) <= cap {
		t.Fatal("demand trace appears to have been mutated by censoring")
	}
}

func TestObservedSeriesUncensoredWhenGenerous(t *testing.T) {
	tr := mustTrace(t, ClassStableCPU)
	cpu, mem := ObservedSeries(tr, tr.GroundTruth.CPUMaxMilli*2, tr.GroundTruth.MemoryMaxBytes*2, DefaultConfig())
	for i := range cpu.Samples {
		if cpu.Samples[i].Value != tr.CPUDemand.Samples[i].Value {
			t.Fatalf("generous CPU config should not censor sample %d", i)
		}
		if mem.Samples[i].Value != tr.MemoryDemand.Samples[i].Value {
			t.Fatalf("generous memory config should not censor sample %d", i)
		}
	}
}

// A workload that is already being OOMKilled at its declared size must surface
// that evidence to the engine, since the OOM gate is what reacts to it.
func TestAsWorkloadSurfacesOOMEvidence(t *testing.T) {
	spec, err := SpecByClass(ClassBurstyMemory, testDuration, testStep, testSeed)
	if err != nil {
		t.Fatal(err)
	}
	// Deploy it deliberately under-sized on memory.
	spec.DeclaredMemory = spec.BaselineMemory * 1.1
	tr, err := Generate(spec)
	if err != nil {
		t.Fatal(err)
	}
	w := AsWorkload(tr, DefaultConfig())
	ev, ok := w.EvidenceFor("app")
	if !ok {
		t.Fatal("expected evidence for container app")
	}
	if ev.OOMKills == 0 {
		t.Error("an under-sized bursty-memory workload should report OOMKills")
	}
	if ev.LastOOM == nil {
		t.Error("expected a LastOOM timestamp")
	}
}

func TestAsWorkloadHealthyHasNoOOMEvidence(t *testing.T) {
	tr := mustTrace(t, ClassStableMemory)
	w := AsWorkload(tr, DefaultConfig())
	ev, _ := w.EvidenceFor("app")
	if ev.OOMKills != 0 {
		t.Errorf("a well-provisioned workload should report no OOMKills, got %d", ev.OOMKills)
	}
	if w.Kind != model.KindSynthetic {
		t.Errorf("simulated workloads must be marked synthetic, got %q", w.Kind)
	}
}

func TestSavingsFraction(t *testing.T) {
	if got := SavingsFraction(100, 60); math.Abs(got-0.4) > 1e-9 {
		t.Errorf("savings 100->60 = %v want 0.4", got)
	}
	if got := SavingsFraction(100, 120); math.Abs(got-(-0.2)) > 1e-9 {
		t.Errorf("savings 100->120 = %v want -0.2", got)
	}
	if got := SavingsFraction(0, 50); got != 0 {
		t.Errorf("savings from zero baseline = %v want 0", got)
	}
}

func TestWindowSubsetsSeries(t *testing.T) {
	tr := mustTrace(t, ClassStableCPU)
	full := tr.CPUDemand
	w := full.Window(time.Hour)
	if w.Duration() > time.Hour+testStep {
		t.Errorf("window duration %v exceeds requested hour", w.Duration())
	}
	if w.Len() >= full.Len() {
		t.Errorf("1h window (%d samples) should be shorter than the 24h trace (%d)", w.Len(), full.Len())
	}
	// The window must be the most recent portion, not the earliest.
	if w.Samples[len(w.Samples)-1].Timestamp != full.Samples[len(full.Samples)-1].Timestamp {
		t.Error("window should end at the latest sample")
	}
	// Requesting more than exists returns everything.
	if got := full.Window(365 * 24 * time.Hour); got.Len() != full.Len() {
		t.Errorf("oversized window returned %d of %d samples", got.Len(), full.Len())
	}
}

func mustTrace(t *testing.T, c Class) Trace {
	t.Helper()
	spec, err := SpecByClass(c, testDuration, testStep, testSeed)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := Generate(spec)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}
