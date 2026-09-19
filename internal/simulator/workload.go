// Package simulator generates synthetic workload traces with known ground
// truth, and replays a candidate resource configuration against a trace to
// measure what would actually have happened.
//
// # Why this exists
//
// The hard problem in evaluating a right-sizing recommendation is that the
// observed usage series is not the workload's demand. It is demand after the
// cluster has already interfered with it: CPU that was throttled was never
// recorded as used, and a container that was OOMKilled never recorded the
// working set it was reaching for. Measured usage is censored by the very
// configuration being evaluated. On a real cluster, comparing a recommendation
// against measured usage therefore cannot distinguish "the recommendation was
// correct" from "the recommendation suppressed the demand that would have
// falsified it".
//
// A generative model separates the two. The generator produces a demand trace —
// what the workload wants, independent of any configuration — and the replay
// evaluator (replay.go) subjects that demand to a candidate configuration and
// derives the consequences: throttled CPU-seconds, OOMKills, restarts. The
// recommendation is then scored against demand it never saw, which is the only
// way to ask whether it was actually good rather than merely smaller.
//
// This is a modelling choice with real costs, stated in research/limitations.md:
// synthetic demand is not production demand, and results obtained this way bound
// what can be claimed. The kind-based end-to-end test (test/e2e) exercises the
// same engine against real Prometheus data from real containers, which validates
// the production path but cannot provide ground truth. The two are complements.
package simulator

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

// Class is a workload behaviour class. The classes are defined by their
// generative structure, not by a classifier applied after the fact, which is
// what makes them ground truth.
type Class string

const (
	// ClassStableCPU has CPU demand fluctuating narrowly around a baseline.
	// The easy case: any sensible percentile policy should do well, so it
	// functions as a control.
	ClassStableCPU Class = "stable-cpu"
	// ClassBurstyCPU has a low baseline punctuated by short, tall bursts at
	// random intervals. This is the case where the percentile choice should
	// matter most, because the bursts occupy a small fraction of samples.
	ClassBurstyCPU Class = "bursty-cpu"
	// ClassPeriodicCPU has a deterministic diurnal cycle plus noise. Its peaks
	// occupy a large fraction of samples, so percentiles should behave very
	// differently here than on bursty-cpu despite similar max/mean ratios.
	ClassPeriodicCPU Class = "periodic-cpu"
	// ClassSpikyCPU has rare, extreme spikes — the adversarial case for any
	// percentile below the maximum.
	ClassSpikyCPU Class = "spiky-cpu"
	// ClassStableMemory has a flat working set with small variation.
	ClassStableMemory Class = "stable-memory"
	// ClassGrowingMemory has a working set that trends upward over the window,
	// as caches fill or a slow leak accumulates. Any policy that summarises the
	// window with a single statistic should be expected to under-provision the
	// future here; the question is by how much.
	ClassGrowingMemory Class = "growing-memory"
	// ClassBurstyMemory has a baseline working set with transient allocations.
	ClassBurstyMemory Class = "bursty-memory"
	// ClassSawtoothMemory rises and drops sharply, as in a garbage-collected
	// runtime. Its maximum is a poor description of its typical state, and its
	// mean is a dangerous one.
	ClassSawtoothMemory Class = "sawtooth-memory"
	// ClassMixed has correlated CPU and memory activity, as in a request-serving
	// service where load drives both.
	ClassMixed Class = "mixed"
	// ClassIdle is nearly unused — the case where percentile policies produce
	// implausibly small requests and the floor gate must intervene.
	ClassIdle Class = "idle"
)

// AllClasses is the evaluated set, in a fixed order so that generated files and
// figures are stable across runs.
func AllClasses() []Class {
	return []Class{
		ClassStableCPU, ClassBurstyCPU, ClassPeriodicCPU, ClassSpikyCPU,
		ClassStableMemory, ClassGrowingMemory, ClassBurstyMemory, ClassSawtoothMemory,
		ClassMixed, ClassIdle,
	}
}

// Spec fully determines a generated workload. Every field that affects the
// output is here, and the struct is serialised into experiment results, so a
// trace can be regenerated exactly from its record.
type Spec struct {
	Class Class  `json:"class"`
	Name  string `json:"name"`

	// Duration is the length of demand to generate.
	Duration time.Duration `json:"duration"`
	// Step is the sampling interval of the generated series. It is also the
	// resolution at which the replay evaluator integrates, so it bounds what
	// the experiment can observe: bursts shorter than Step are not
	// representable. Documented as a measurement-validity threat.
	Step time.Duration `json:"step"`

	// CPU demand parameters, in millicores.
	BaselineCPU  float64 `json:"baseline_cpu_milli"`
	BurstCPU     float64 `json:"burst_cpu_milli"`
	CPUNoiseFrac float64 `json:"cpu_noise_frac"`

	// Memory demand parameters, in bytes.
	BaselineMemory  float64 `json:"baseline_memory_bytes"`
	PeakMemory      float64 `json:"peak_memory_bytes"`
	MemoryNoiseFrac float64 `json:"memory_noise_frac"`

	// Burst/cycle shape.
	BurstDuration time.Duration `json:"burst_duration"`
	BurstInterval time.Duration `json:"burst_interval"`
	// BurstJitter scales the randomness of burst arrival: 0 makes bursts
	// perfectly periodic, 1 makes intervals exponentially distributed with the
	// given mean. Bursty and periodic classes differ mainly in this parameter,
	// which lets the study separate "has peaks" from "has predictable peaks".
	BurstJitter float64       `json:"burst_jitter"`
	CycleLength time.Duration `json:"cycle_length"`

	// GrowthFactor is the multiplicative growth of the memory working set over
	// the whole duration (1.0 = flat).
	GrowthFactor float64 `json:"growth_factor"`

	// Declared requests the workload is deployed with, used as Baseline A and
	// as the denominator for savings. Over-provisioning here is the waste the
	// engine is asked to find (RQ1).
	DeclaredCPU    float64 `json:"declared_cpu_milli"`
	DeclaredMemory float64 `json:"declared_memory_bytes"`

	// Seed makes generation deterministic.
	Seed int64 `json:"seed"`
}

// Trace is a generated workload: its demand series plus the spec that produced
// it. Demand is what the workload wants; it is never modified by replay.
type Trace struct {
	Spec Spec `json:"spec"`
	// CPUDemand is in millicores, MemoryDemand in bytes.
	CPUDemand    model.Series `json:"-"`
	MemoryDemand model.Series `json:"-"`
	// GroundTruth is the demand envelope derived analytically from the spec
	// where possible and empirically from the trace otherwise.
	GroundTruth GroundTruth `json:"ground_truth"`
}

// GroundTruth is the resource envelope the workload actually requires, computed
// from the demand trace rather than from any observed (censored) series.
//
// This is the reference a recommendation is scored against. "Required" is stated
// at several quantiles because there is no single correct answer: a
// configuration meeting the p99 of demand is a defensible operating point, and
// so is one meeting the maximum; they differ in cost and in risk, which is
// precisely the trade-off under study (RQ5).
type GroundTruth struct {
	CPUMeanMilli float64 `json:"cpu_mean_milli"`
	CPUP95Milli  float64 `json:"cpu_p95_milli"`
	CPUP99Milli  float64 `json:"cpu_p99_milli"`
	CPUMaxMilli  float64 `json:"cpu_max_milli"`

	MemoryMeanBytes float64 `json:"memory_mean_bytes"`
	MemoryP95Bytes  float64 `json:"memory_p95_bytes"`
	MemoryP99Bytes  float64 `json:"memory_p99_bytes"`
	MemoryMaxBytes  float64 `json:"memory_max_bytes"`

	// CPUDemandSeconds is the integral of CPU demand over the trace, in
	// core-seconds. It is the denominator for the throttling metric: throttled
	// work is meaningful only as a fraction of work demanded.
	CPUDemandSeconds float64 `json:"cpu_demand_core_seconds"`
}

// Generate produces a deterministic trace from a spec.
//
// Determinism is per-spec, not global: the RNG is seeded from Spec.Seed alone,
// so generating workload 5 of a batch does not depend on having generated
// workloads 1-4. Without that property, adding a workload class to the matrix
// would silently change every existing result.
func Generate(spec Spec) (Trace, error) {
	if err := spec.validate(); err != nil {
		return Trace{}, err
	}
	rng := rand.New(rand.NewSource(spec.Seed))
	n := int(spec.Duration/spec.Step) + 1
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC) // fixed epoch: a Monday

	cpu := make([]model.Sample, n)
	mem := make([]model.Sample, n)

	burstStarts := burstSchedule(spec, rng)

	for i := 0; i < n; i++ {
		t := start.Add(time.Duration(i) * spec.Step)
		elapsed := time.Duration(i) * spec.Step
		progress := float64(elapsed) / float64(spec.Duration)

		c, m := spec.demandAt(elapsed, progress, burstStarts, rng)
		cpu[i] = model.Sample{Timestamp: t, Value: math.Max(0, c)}
		mem[i] = model.Sample{Timestamp: t, Value: math.Max(0, m)}
	}

	tr := Trace{
		Spec:         spec,
		CPUDemand:    model.Series{Resource: model.ResourceCPU, Samples: cpu, Step: spec.Step},
		MemoryDemand: model.Series{Resource: model.ResourceMemory, Samples: mem, Step: spec.Step},
	}
	tr.GroundTruth = ComputeGroundTruth(tr)
	return tr, nil
}

func (s Spec) validate() error {
	switch {
	case s.Duration <= 0:
		return fmt.Errorf("spec %q: duration must be positive", s.Name)
	case s.Step <= 0:
		return fmt.Errorf("spec %q: step must be positive", s.Name)
	case s.Step > s.Duration:
		return fmt.Errorf("spec %q: step %s exceeds duration %s", s.Name, s.Step, s.Duration)
	case s.BaselineCPU < 0 || s.BurstCPU < 0:
		return fmt.Errorf("spec %q: CPU demand must be non-negative", s.Name)
	case s.BaselineMemory < 0 || s.PeakMemory < 0:
		return fmt.Errorf("spec %q: memory demand must be non-negative", s.Name)
	}
	return nil
}

// burstSchedule precomputes burst start offsets.
//
// With BurstJitter == 0 bursts are exactly periodic. With jitter > 0 the gaps
// are drawn from an exponential distribution scaled by the jitter, blended with
// the deterministic interval. Precomputing the schedule keeps demandAt a pure
// function of time given the schedule, which makes the trace reproducible
// regardless of how the sample loop is ordered or parallelised.
func burstSchedule(s Spec, rng *rand.Rand) []time.Duration {
	if s.BurstInterval <= 0 || s.BurstDuration <= 0 {
		return nil
	}
	var out []time.Duration
	for t := time.Duration(0); t < s.Duration; {
		out = append(out, t)
		gap := s.BurstInterval
		if s.BurstJitter > 0 {
			// Exponential gap with mean BurstInterval, blended by jitter so that
			// jitter=1 is fully memoryless and jitter=0.3 is mildly irregular.
			exp := time.Duration(rng.ExpFloat64() * float64(s.BurstInterval))
			gap = time.Duration((1-s.BurstJitter)*float64(s.BurstInterval) + s.BurstJitter*float64(exp))
		}
		if gap < s.Step {
			gap = s.Step
		}
		t += gap
	}
	return out
}

func (s Spec) inBurst(elapsed time.Duration, starts []time.Duration) bool {
	// Bursts are sparse and sorted, so a binary search keeps generation linear
	// even for 7-day traces at 30s resolution (20k samples) with many bursts.
	i := sort.Search(len(starts), func(i int) bool { return starts[i] > elapsed })
	if i == 0 {
		return false
	}
	return elapsed < starts[i-1]+s.BurstDuration
}

// demandAt evaluates the generative model at one point in time.
//
// Noise is multiplicative and truncated at zero rather than additive Gaussian,
// because resource demand is a positive quantity whose variability scales with
// its level: a service using 2 cores does not fluctuate by the same absolute
// amount as one using 20 millicores.
func (s Spec) demandAt(elapsed time.Duration, progress float64, bursts []time.Duration, rng *rand.Rand) (cpu, mem float64) {
	noise := func(frac float64) float64 {
		if frac <= 0 {
			return 1.0
		}
		return math.Max(0.05, 1.0+rng.NormFloat64()*frac)
	}

	switch s.Class {
	case ClassStableCPU:
		cpu = s.BaselineCPU * noise(s.CPUNoiseFrac)
		mem = s.BaselineMemory * noise(s.MemoryNoiseFrac)

	case ClassBurstyCPU, ClassSpikyCPU:
		cpu = s.BaselineCPU * noise(s.CPUNoiseFrac)
		if s.inBurst(elapsed, bursts) {
			cpu = s.BurstCPU * noise(s.CPUNoiseFrac*0.5)
		}
		mem = s.BaselineMemory * noise(s.MemoryNoiseFrac)

	case ClassPeriodicCPU:
		// A raised cosine over the cycle: smooth, deterministic, and with peaks
		// that occupy a large fraction of the period, unlike the burst classes.
		phase := 0.0
		if s.CycleLength > 0 {
			phase = 2 * math.Pi * math.Mod(elapsed.Seconds(), s.CycleLength.Seconds()) / s.CycleLength.Seconds()
		}
		shape := 0.5 * (1 - math.Cos(phase)) // in [0,1]
		cpu = (s.BaselineCPU + (s.BurstCPU-s.BaselineCPU)*shape) * noise(s.CPUNoiseFrac)
		mem = s.BaselineMemory * noise(s.MemoryNoiseFrac)

	case ClassStableMemory:
		cpu = s.BaselineCPU * noise(s.CPUNoiseFrac)
		mem = s.BaselineMemory * noise(s.MemoryNoiseFrac)

	case ClassGrowingMemory:
		cpu = s.BaselineCPU * noise(s.CPUNoiseFrac)
		g := 1.0
		if s.GrowthFactor > 0 {
			g = 1 + (s.GrowthFactor-1)*progress
		}
		mem = s.BaselineMemory * g * noise(s.MemoryNoiseFrac)

	case ClassBurstyMemory:
		cpu = s.BaselineCPU * noise(s.CPUNoiseFrac)
		mem = s.BaselineMemory * noise(s.MemoryNoiseFrac)
		if s.inBurst(elapsed, bursts) {
			mem = s.PeakMemory * noise(s.MemoryNoiseFrac*0.5)
		}

	case ClassSawtoothMemory:
		// Linear accumulation to the peak then an instantaneous drop, as in a
		// heap filling between garbage collections.
		cycle := s.CycleLength
		if cycle <= 0 {
			cycle = 30 * time.Minute
		}
		frac := math.Mod(elapsed.Seconds(), cycle.Seconds()) / cycle.Seconds()
		mem = (s.BaselineMemory + (s.PeakMemory-s.BaselineMemory)*frac) * noise(s.MemoryNoiseFrac)
		cpu = s.BaselineCPU * noise(s.CPUNoiseFrac)
		// Collection itself costs CPU, so the CPU series is correlated with the
		// end of each memory cycle.
		if frac > 0.95 {
			cpu = s.BurstCPU * noise(s.CPUNoiseFrac)
		}

	case ClassMixed:
		// Correlated load: one latent demand signal drives both resources, as in
		// a request-serving service. Memory responds with damped amplitude
		// because working sets are stickier than CPU.
		phase := 0.0
		if s.CycleLength > 0 {
			phase = 2 * math.Pi * math.Mod(elapsed.Seconds(), s.CycleLength.Seconds()) / s.CycleLength.Seconds()
		}
		load := 0.5 * (1 - math.Cos(phase))
		if s.inBurst(elapsed, bursts) {
			load = math.Min(1.0, load+0.6)
		}
		cpu = (s.BaselineCPU + (s.BurstCPU-s.BaselineCPU)*load) * noise(s.CPUNoiseFrac)
		mem = (s.BaselineMemory + (s.PeakMemory-s.BaselineMemory)*load*0.6) * noise(s.MemoryNoiseFrac)

	case ClassIdle:
		cpu = s.BaselineCPU * noise(s.CPUNoiseFrac)
		mem = s.BaselineMemory * noise(s.MemoryNoiseFrac)

	default:
		cpu = s.BaselineCPU
		mem = s.BaselineMemory
	}
	return cpu, mem
}

// ComputeGroundTruth derives the demand envelope from a trace. It is exported so
// that the experiment runner can recompute ground truth for a sub-trace: when a
// trace is split into a fitting window and a held-out horizon, the horizon must
// be scored against its own envelope, not the whole trace's.
func ComputeGroundTruth(tr Trace) GroundTruth {
	cpuVals := tr.CPUDemand.Values()
	memVals := tr.MemoryDemand.Values()
	gt := GroundTruth{}
	if len(cpuVals) > 0 {
		gt.CPUMeanMilli = mean(cpuVals)
		gt.CPUP95Milli = pctl(cpuVals, 0.95)
		gt.CPUP99Milli = pctl(cpuVals, 0.99)
		gt.CPUMaxMilli = maxOf(cpuVals)
		// Integrate demand: millicores * step, converted to core-seconds.
		step := tr.Spec.Step.Seconds()
		for _, v := range cpuVals {
			gt.CPUDemandSeconds += v / 1000.0 * step
		}
	}
	if len(memVals) > 0 {
		gt.MemoryMeanBytes = mean(memVals)
		gt.MemoryP95Bytes = pctl(memVals, 0.95)
		gt.MemoryP99Bytes = pctl(memVals, 0.99)
		gt.MemoryMaxBytes = maxOf(memVals)
	}
	return gt
}
