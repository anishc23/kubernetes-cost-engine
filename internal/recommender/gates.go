package recommender

import (
	"fmt"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/internal/stats"
)

// GateInput is everything a gate may consult. It deliberately includes the
// current request and the reliability evidence, which strategies may not see.
type GateInput struct {
	Resource ResourceKindView
	// Current is the declared request in canonical units.
	Current float64
	// Target is the value produced by the strategy (possibly already modified
	// by an earlier gate).
	Target float64
	// Summary is the observed-usage summary over the window.
	Summary stats.Summary
	// Evidence is the reliability history; Present is false when none was
	// collected, which must make the engine more conservative rather than less.
	Evidence        model.RestartEvidence
	EvidencePresent bool
	// Sufficiency is the outcome of the data-sufficiency check.
	Sufficiency model.DataSufficiency
}

// ResourceKindView is the resource a gate is evaluating.
type ResourceKindView = model.ResourceKind

// GateResult is a gate's verdict.
type GateResult struct {
	// Target is the (possibly adjusted) target the gate passes on.
	Target float64
	// Block is set when the gate refuses any reduction below Current.
	Block bool
	// Fired indicates the gate changed something, for reporting and for the
	// optimizer_gate_fired_total metric.
	Fired bool
	// Reason explains the verdict in one sentence.
	Reason string
}

// Gate is a safety rule evaluated after a strategy has produced a target.
//
// Gates form an ordered chain. Each may raise the target or block a reduction
// outright; by construction none may lower it. That invariant is enforced by a
// test (TestGatesNeverLowerTarget) rather than by convention, because a gate
// that lowered a target would convert a safety mechanism into a risk.
type Gate interface {
	ID() string
	Evaluate(in GateInput) GateResult
}

// --- Data sufficiency -------------------------------------------------------

// SufficiencyGate blocks any change when the observation window does not
// contain enough data to support one.
//
// This is the first gate for a reason: with three samples, a p99 is arithmetic
// applied to noise. Reporting INSUFFICIENT_DATA is more useful to an operator
// than a confident-looking number, and it prevents a newly deployed workload —
// whose first minutes of usage are unrepresentative — from being shrunk.
type SufficiencyGate struct {
	MinSamples  int
	MinDuration time.Duration
	// MinCoverage is the minimum fraction of expected samples that must be
	// present. Prometheus gaps (scrape failures, restarts) produce series that
	// span the full window but contain far fewer points than expected; without
	// a coverage check such a series looks sufficient while describing only a
	// fraction of the period.
	MinCoverage float64
}

func (g SufficiencyGate) ID() string { return "data-sufficiency" }

func (g SufficiencyGate) Evaluate(in GateInput) GateResult {
	if in.Sufficiency.Sufficient {
		return GateResult{Target: in.Target}
	}
	return GateResult{
		Target: in.Current,
		Block:  true,
		Fired:  true,
		Reason: fmt.Sprintf(
			"insufficient data: %d samples over %s (need %d samples, %s, %.0f%% coverage; observed %.0f%%)",
			in.Sufficiency.Samples, in.Sufficiency.Observed.Round(time.Minute),
			in.Sufficiency.MinSamples, in.Sufficiency.MinObserved,
			g.MinCoverage*100, in.Sufficiency.Coverage*100),
	}
}

// --- OOM protection --------------------------------------------------------

// OOMProtectionGate blocks memory reductions when there is evidence that the
// workload has already been OOMKilled, and blocks them when no evidence could be
// collected at all.
//
// The asymmetry with CPU is the central systems argument of this project. A CPU
// request set too low degrades latency and is recoverable by raising it; a
// memory limit set too low kills the process. An OOMKill in the observation
// window is direct evidence that the workload's true memory envelope was
// underestimated at least once, and the observed working-set series cannot show
// how much memory the process would have used had it not been killed — the
// series is censored at exactly the point of interest. Reducing memory on such a
// workload is therefore not a calculated risk but an uninformed one.
type OOMProtectionGate struct {
	// LookbackRelevance is how recent an OOMKill must be to block. An OOMKill
	// from six weeks ago in a 7-day window is not evidence about current
	// behaviour; one inside the window is.
	LookbackRelevance time.Duration
	// RequireEvidence blocks memory reductions when no restart/OOM evidence
	// could be collected. Defaults on: absent evidence is not evidence of
	// absence.
	RequireEvidence bool
}

func (g OOMProtectionGate) ID() string { return "oom-protection" }

func (g OOMProtectionGate) Evaluate(in GateInput) GateResult {
	if in.Resource != model.ResourceMemory {
		return GateResult{Target: in.Target}
	}
	reducing := in.Target < in.Current
	if !reducing {
		// Increases and no-ops are always allowed: this gate exists to prevent
		// unsafe reductions, not to freeze the workload.
		return GateResult{Target: in.Target}
	}
	if !in.EvidencePresent {
		if g.RequireEvidence {
			return GateResult{
				Target: in.Current, Block: true, Fired: true,
				Reason: "memory reduction withheld: no restart or OOM evidence available for this container",
			}
		}
		return GateResult{Target: in.Target}
	}
	if in.Evidence.OOMKills > 0 {
		recent := true
		if in.Evidence.LastOOM != nil && g.LookbackRelevance > 0 {
			recent = time.Since(*in.Evidence.LastOOM) <= g.LookbackRelevance
		}
		if recent {
			return GateResult{
				Target: in.Current, Block: true, Fired: true,
				Reason: fmt.Sprintf(
					"memory reduction withheld: %d OOMKill(s) observed, so the working-set series is censored and understates true demand",
					in.Evidence.OOMKills),
			}
		}
	}
	return GateResult{Target: in.Target}
}

// --- Usage-exceeds-request gate --------------------------------------------

// UsageExceedsRequestGate blocks a reduction when observed usage already
// reaches or exceeds the current request.
//
// Such a workload is not over-provisioned in any sense that would justify a
// reduction, and for CPU this is the signal that the container has been
// competing for time above its request. The threshold is on a high percentile
// rather than the maximum, so that a single scrape artefact does not veto an
// otherwise well-supported recommendation.
type UsageExceedsRequestGate struct {
	// Percentile selects which observed statistic is compared against the
	// current request: "p95", "p99" or "max".
	Percentile string
}

func (g UsageExceedsRequestGate) ID() string { return "usage-exceeds-request" }

func (g UsageExceedsRequestGate) Evaluate(in GateInput) GateResult {
	if in.Target >= in.Current {
		return GateResult{Target: in.Target}
	}
	var observed float64
	switch g.Percentile {
	case "p95":
		observed = in.Summary.P95
	case "max":
		observed = in.Summary.Max
	default:
		observed = in.Summary.P99
	}
	if observed >= in.Current {
		return GateResult{
			Target: in.Current, Block: true, Fired: true,
			Reason: fmt.Sprintf(
				"reduction withheld: observed %s usage (%.0f) already meets or exceeds the current request (%.0f)",
				g.Percentile, observed, in.Current),
		}
	}
	return GateResult{Target: in.Target}
}

// --- Floor gate ------------------------------------------------------------

// FloorGate raises targets that fall below a configured absolute minimum.
//
// Percentile policies applied to a nearly idle workload produce arbitrarily
// small requests. A 1m CPU request is not a useful recommendation: it is below
// the granularity at which CFS enforces quota, and it leaves no headroom for
// process startup, which is often the busiest moment in a container's life.
type FloorGate struct {
	CPUFloorMilli    float64
	MemoryFloorBytes float64
}

func (g FloorGate) ID() string { return "floor" }

func (g FloorGate) Evaluate(in GateInput) GateResult {
	floor := g.CPUFloorMilli
	if in.Resource == model.ResourceMemory {
		floor = g.MemoryFloorBytes
	}
	if floor > 0 && in.Target < floor {
		return GateResult{
			Target: floor, Fired: true,
			Reason: fmt.Sprintf("target raised to the configured %s floor (%.0f)", in.Resource, floor),
		}
	}
	return GateResult{Target: in.Target}
}

// --- Instability gate ------------------------------------------------------

// InstabilityGate blocks reductions for workloads whose usage is too erratic
// for a percentile estimate over the available window to be meaningful.
//
// Burstiness (max/mean) is used rather than the coefficient of variation
// because the failure mode being guarded against is a tall rare peak, which
// inflates max/mean sharply while barely moving the standard deviation when the
// peak is short.
type InstabilityGate struct {
	// MaxBurstiness is the max/mean ratio above which reductions are withheld.
	// Zero disables the gate.
	MaxBurstiness float64
	// AppliesTo restricts the gate to specific resources. Empty means both.
	AppliesTo []model.ResourceKind
}

func (g InstabilityGate) ID() string { return "instability" }

func (g InstabilityGate) Evaluate(in GateInput) GateResult {
	if g.MaxBurstiness <= 0 || in.Target >= in.Current {
		return GateResult{Target: in.Target}
	}
	if len(g.AppliesTo) > 0 {
		applies := false
		for _, r := range g.AppliesTo {
			if r == in.Resource {
				applies = true
			}
		}
		if !applies {
			return GateResult{Target: in.Target}
		}
	}
	if in.Summary.Burstiness > g.MaxBurstiness {
		return GateResult{
			Target: in.Current, Block: true, Fired: true,
			Reason: fmt.Sprintf(
				"reduction withheld: usage burstiness %.1fx exceeds the %.1fx threshold for a stable estimate",
				in.Summary.Burstiness, g.MaxBurstiness),
		}
	}
	return GateResult{Target: in.Target}
}

// --- Minimum-change gate ---------------------------------------------------

// MinChangeGate suppresses changes smaller than a relative threshold.
//
// Applying a 3% request change requires a rollout: every pod in the workload is
// recreated. The churn is rarely worth the saving, and a system that emits such
// recommendations trains operators to ignore it. This gate is what turns a
// marginal DECREASE into a NO_CHANGE.
type MinChangeGate struct {
	// MinRelativeChange is the minimum |target-current|/current to act on.
	MinRelativeChange float64
}

func (g MinChangeGate) ID() string { return "min-change" }

func (g MinChangeGate) Evaluate(in GateInput) GateResult {
	if in.Current <= 0 || g.MinRelativeChange <= 0 {
		return GateResult{Target: in.Target}
	}
	rel := (in.Target - in.Current) / in.Current
	if rel < 0 {
		rel = -rel
	}
	if rel < g.MinRelativeChange {
		return GateResult{
			Target: in.Current, Fired: true,
			Reason: fmt.Sprintf(
				"no change: proposed adjustment of %.1f%% is below the %.1f%% threshold that justifies a rollout",
				rel*100, g.MinRelativeChange*100),
		}
	}
	return GateResult{Target: in.Target}
}
