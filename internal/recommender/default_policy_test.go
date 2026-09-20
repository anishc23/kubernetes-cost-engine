package recommender

import (
	"testing"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

// The shipped default is an experimental result, not a preference. These tests
// pin the properties the experiments established, so that a future change to the
// default has to confront the evidence rather than silently discard it. Each
// assertion names the finding it encodes; see research/results.md.
func TestDefaultPolicyEncodesExperimentalFindings(t *testing.T) {
	p := DefaultPolicy()

	// Finding: CPU p95 left 34% of demanded work unserved on the bursty class
	// when measured against held-out demand. p99 is the lowest percentile that
	// was safe on every class except the spiky one.
	if p.CPUStrategy != "p99" {
		t.Errorf("default CPU strategy = %q; the experiments refuted percentiles below p99 "+
			"(p95 left 34%% of demanded work unserved on bursty-cpu). Changing this requires new evidence.",
			p.CPUStrategy)
	}

	// Finding: memory p95 plateaus below full survival at any margin, because on
	// bursty-memory its p95 is a small fraction of the peak. Only max reached
	// full survival.
	if p.MemoryStrategy != "max" {
		t.Errorf("default memory strategy = %q; only max reached full survival across classes, "+
			"and p95 did not even at a 2.0x margin", p.MemoryStrategy)
	}

	// Finding: max x1.0 survived only 47% of conditions, because the maximum
	// observed in the fitting window is not an upper bound on the next period.
	// Full survival required a margin of at least 1.2.
	if p.MemorySafetyFactor < 1.2 {
		t.Errorf("default memory safety factor = %.2f; the observed maximum alone survived only 47%% "+
			"of conditions out of sample, and full survival required at least 1.2x", p.MemorySafetyFactor)
	}

	// Finding: observed burstiness separates the one class where p99 fails
	// (spiky, 38-43) from every class where it succeeds (highest 13.0).
	if p.MaxCPUBurstiness <= 14 || p.MaxCPUBurstiness >= 38 {
		t.Errorf("default CPU burstiness threshold = %.0f; it must sit between the highest safe class "+
			"(bursty-cpu at 13.0) and the failing one (spiky-cpu at 38) to withhold reductions on exactly "+
			"the workloads where a p99 policy under-provisions", p.MaxCPUBurstiness)
	}

	// Mutation must never be implied by the policy itself, and OOM protection
	// must be on: both are safety stances, not tuning.
	if !p.OOMProtection || !p.OOMRequireEvidence {
		t.Error("OOM protection and its require-evidence mode must be enabled by default")
	}

	if err := p.Validate(NewRegistry(p.PercentileMethod)); err != nil {
		t.Errorf("the shipped default must be a valid policy: %v", err)
	}
}

// The default must actually behave conservatively on the shape of workload that
// motivated it: an erratic series must not be reduced.
func TestDefaultPolicyWithholdsReductionOnErraticCPU(t *testing.T) {
	e, err := NewEngine(DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy()
	p.MinSamples = 20
	p.MinDuration = 10 * time.Minute
	e, err = NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}

	// A spiky series: a low baseline with one very tall, very rare peak.
	// burstiness = max/mean is far above the threshold.
	vals := append(repeat(50, 199), 3000)
	cpu := series(model.ResourceCPU, 30*time.Second, vals...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(300e6, 200)...)
	ev := model.RestartEvidence{}
	rec := e.Recommend(workload(cpu, mem, 3000, 4e9, &ev))

	c := rec.Containers[0]
	if c.CPU.Decision != model.DecisionBlocked {
		t.Errorf("CPU decision on a spiky series = %s, want BLOCKED (burstiness %.1f, reason: %s)",
			c.CPU.Decision, c.CPU.Stats.Burstiness, c.CPU.Reason)
	}
	// Memory is unaffected: the instability gate applies to CPU only.
	if c.Memory.Decision != model.DecisionDecrease {
		t.Errorf("memory decision = %s; the CPU instability gate must not block memory", c.Memory.Decision)
	}
}

// And it must still act on the ordinary case, or a conservative default would
// simply be a broken one.
func TestDefaultPolicyStillReducesStableWorkloads(t *testing.T) {
	p := DefaultPolicy()
	p.MinSamples = 20
	p.MinDuration = 10 * time.Minute
	e, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	cpu := series(model.ResourceCPU, 30*time.Second, repeat(200, 200)...)
	mem := series(model.ResourceMemory, 30*time.Second, repeat(300e6, 200)...)
	ev := model.RestartEvidence{}
	rec := e.Recommend(workload(cpu, mem, 2000, 4e9, &ev))
	c := rec.Containers[0]
	if c.CPU.Decision != model.DecisionDecrease {
		t.Errorf("CPU decision on a stable over-provisioned workload = %s, want DECREASE (reason: %s)",
			c.CPU.Decision, c.CPU.Reason)
	}
	if c.Memory.Decision != model.DecisionDecrease {
		t.Errorf("memory decision = %s, want DECREASE", c.Memory.Decision)
	}
	// The CPU target must clear the observed maximum with its margin, since the
	// default is p99 x1.25 on a near-constant series.
	if c.CPU.Target < c.CPU.Stats.Max {
		t.Errorf("CPU target %v should exceed the observed max %v at a 1.25x margin",
			c.CPU.Target, c.CPU.Stats.Max)
	}
}
