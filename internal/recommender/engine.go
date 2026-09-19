package recommender

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/internal/stats"
	"github.com/anishc23/k8s-cost-optimizer/pkg/quantity"
)

// Policy is the complete configuration of the recommendation engine.
//
// It is a value type with a content-addressed ID (see PolicyID) so that any
// recommendation, in production or in an experiment, can be traced back to the
// exact configuration that produced it. This is the mechanism that makes the
// reproducibility claim in research/methodology.md checkable rather than
// aspirational.
type Policy struct {
	// CPUStrategy and MemoryStrategy are strategy IDs from the Registry. They
	// are separate fields, not one setting, because the project's core claim is
	// that CPU and memory require different policies.
	CPUStrategy    string `json:"cpu_strategy"`
	MemoryStrategy string `json:"memory_strategy"`

	CPUSafetyFactor    float64 `json:"cpu_safety_factor"`
	MemorySafetyFactor float64 `json:"memory_safety_factor"`

	PercentileMethod stats.PercentileMethod `json:"percentile_method"`

	ObservationWindow time.Duration `json:"observation_window"`

	// Data sufficiency thresholds.
	MinSamples  int           `json:"min_samples"`
	MinDuration time.Duration `json:"min_duration"`
	MinCoverage float64       `json:"min_coverage"`

	// Safety gate configuration.
	OOMProtection        bool          `json:"oom_protection"`
	OOMRequireEvidence   bool          `json:"oom_require_evidence"`
	OOMLookbackRelevance time.Duration `json:"oom_lookback_relevance"`

	UsageExceedsRequestPercentile string `json:"usage_exceeds_request_percentile"`

	CPUFloorMilli    float64 `json:"cpu_floor_milli"`
	MemoryFloorBytes float64 `json:"memory_floor_bytes"`

	// MaxCPUBurstiness and MaxMemoryBurstiness disable reductions for erratic
	// workloads. Zero disables the check for that resource. They are separate
	// because memory working sets are ordinarily far smoother than CPU series,
	// so one shared threshold would either be useless for CPU or paralysing for
	// memory.
	MaxCPUBurstiness    float64 `json:"max_cpu_burstiness"`
	MaxMemoryBurstiness float64 `json:"max_memory_burstiness"`

	MinRelativeChange float64 `json:"min_relative_change"`

	// Rounding may be disabled in experiments that isolate the statistical
	// policy from the quantisation applied on top of it.
	Rounding bool `json:"rounding"`
}

// DefaultPolicy is the engine's shipped configuration.
//
// The values encode the conservative stance documented in docs/security.md and
// research/methodology.md: p95 with a 1.15x margin for CPU (compressible,
// recoverable) and the observed maximum with a 1.25x margin for memory
// (incompressible, fatal). These are defaults to be evaluated by the
// experiments, not conclusions from them; results.md reports whether they hold
// up per workload class.
func DefaultPolicy() Policy {
	return Policy{
		CPUStrategy:                   "p95",
		MemoryStrategy:                "max",
		CPUSafetyFactor:               1.15,
		MemorySafetyFactor:            1.25,
		PercentileMethod:              stats.LinearInterpolation,
		ObservationWindow:             7 * 24 * time.Hour,
		MinSamples:                    60,
		MinDuration:                   time.Hour,
		MinCoverage:                   0.80,
		OOMProtection:                 true,
		OOMRequireEvidence:            true,
		OOMLookbackRelevance:          14 * 24 * time.Hour,
		UsageExceedsRequestPercentile: "p99",
		CPUFloorMilli:                 10,
		MemoryFloorBytes:              32 * 1024 * 1024,
		MaxCPUBurstiness:              0, // disabled by default; studied as an ablation
		MaxMemoryBurstiness:           0,
		MinRelativeChange:             0.10,
		Rounding:                      true,
	}
}

// PolicyID is a short content hash of the policy, used to label results.
func (p Policy) PolicyID() string {
	b, err := json.Marshal(p)
	if err != nil {
		// Policy contains only scalars and strings; marshalling cannot fail.
		panic(fmt.Sprintf("marshal policy: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// Validate checks the policy for values that would produce meaningless output.
//
// Safety factors below 1.0 are rejected rather than clamped: a factor of 0.9
// means "recommend less than the statistic you just decided was the safe
// envelope", which is more likely a configuration mistake than an intention, and
// silently correcting it would hide the mistake.
func (p Policy) Validate(reg *Registry) error {
	var errs []string
	if _, err := reg.Get(p.CPUStrategy, p.CPUSafetyFactor); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := reg.Get(p.MemoryStrategy, p.MemorySafetyFactor); err != nil {
		errs = append(errs, err.Error())
	}
	if p.CPUSafetyFactor < 1.0 {
		errs = append(errs, fmt.Sprintf("cpu_safety_factor %.2f is below 1.0", p.CPUSafetyFactor))
	}
	if p.MemorySafetyFactor < 1.0 {
		errs = append(errs, fmt.Sprintf("memory_safety_factor %.2f is below 1.0", p.MemorySafetyFactor))
	}
	if p.ObservationWindow <= 0 {
		errs = append(errs, "observation_window must be positive")
	}
	if p.MinCoverage < 0 || p.MinCoverage > 1 {
		errs = append(errs, fmt.Sprintf("min_coverage %.2f is outside [0,1]", p.MinCoverage))
	}
	if p.MinRelativeChange < 0 || p.MinRelativeChange >= 1 {
		errs = append(errs, fmt.Sprintf("min_relative_change %.2f is outside [0,1)", p.MinRelativeChange))
	}
	switch p.UsageExceedsRequestPercentile {
	case "", "p95", "p99", "max":
	default:
		errs = append(errs, fmt.Sprintf("usage_exceeds_request_percentile %q must be p95, p99 or max",
			p.UsageExceedsRequestPercentile))
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid policy: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Engine produces recommendations for workloads under a fixed policy.
type Engine struct {
	policy   Policy
	registry *Registry
	cpu      Strategy
	memory   Strategy
	gates    []Gate
	now      func() time.Time
}

// NewEngine builds an engine. The policy is validated, so a caller holding an
// *Engine can rely on it being consistent.
func NewEngine(p Policy) (*Engine, error) {
	reg := NewRegistry(p.PercentileMethod)
	if err := p.Validate(reg); err != nil {
		return nil, err
	}
	e := &Engine{
		policy:   p,
		registry: reg,
		cpu:      reg.MustGet(p.CPUStrategy, p.CPUSafetyFactor),
		memory:   reg.MustGet(p.MemoryStrategy, p.MemorySafetyFactor),
		now:      time.Now,
	}
	e.gates = buildGates(p)
	return e, nil
}

// buildGates assembles the gate chain in evaluation order.
//
// Order matters and is not arbitrary:
//  1. sufficiency  — if the data cannot support a decision, nothing else applies.
//  2. oom          — the hardest safety constraint, evaluated before cosmetics.
//  3. usage-exceeds — a workload already at its request is not over-provisioned.
//  4. instability  — erratic series make percentile estimates unreliable.
//  5. floor        — raise implausibly small targets.
//  6. min-change   — last, so it sees the final target and can collapse a
//     marginal change to NO_CHANGE.
func buildGates(p Policy) []Gate {
	gates := []Gate{
		SufficiencyGate{MinSamples: p.MinSamples, MinDuration: p.MinDuration, MinCoverage: p.MinCoverage},
	}
	if p.OOMProtection {
		gates = append(gates, OOMProtectionGate{
			LookbackRelevance: p.OOMLookbackRelevance,
			RequireEvidence:   p.OOMRequireEvidence,
		})
	}
	if p.UsageExceedsRequestPercentile != "" {
		gates = append(gates, UsageExceedsRequestGate{Percentile: p.UsageExceedsRequestPercentile})
	}
	if p.MaxCPUBurstiness > 0 {
		gates = append(gates, InstabilityGate{
			MaxBurstiness: p.MaxCPUBurstiness,
			AppliesTo:     []model.ResourceKind{model.ResourceCPU},
		})
	}
	if p.MaxMemoryBurstiness > 0 {
		gates = append(gates, InstabilityGate{
			MaxBurstiness: p.MaxMemoryBurstiness,
			AppliesTo:     []model.ResourceKind{model.ResourceMemory},
		})
	}
	gates = append(gates,
		FloorGate{CPUFloorMilli: p.CPUFloorMilli, MemoryFloorBytes: p.MemoryFloorBytes},
		MinChangeGate{MinRelativeChange: p.MinRelativeChange},
	)
	return gates
}

// Policy returns the engine's configuration.
func (e *Engine) Policy() Policy { return e.policy }

// SetClock overrides the clock, for tests and for replaying historical data.
func (e *Engine) SetClock(f func() time.Time) { e.now = f }

// Recommend produces the full recommendation for a workload.
//
// The workload's series are windowed to the policy's observation window here, so
// that a caller may hand the engine a longer history and let the policy decide
// how much of it to use. This is what makes the observation-window study (RQ6) a
// change of one policy field rather than a change of data collection.
func (e *Engine) Recommend(w model.Workload) model.WorkloadRecommendation {
	rec := model.WorkloadRecommendation{
		Namespace:   w.Namespace,
		Name:        w.Name,
		Kind:        w.Kind,
		Replicas:    w.Replicas,
		GeneratedAt: e.now(),
		PolicyID:    e.policy.PolicyID(),
	}
	for _, c := range w.Containers {
		ev, present := w.EvidenceFor(c.Name)
		cr := model.ContainerRecommendation{Container: c.Name}
		cr.CPU = e.recommendResource(model.ResourceCPU, float64(c.Declared.CPURequest),
			c.CPU.Window(e.policy.ObservationWindow), e.cpu, ev, present)
		cr.Memory = e.recommendResource(model.ResourceMemory, float64(c.Declared.MemoryRequest),
			c.Memory.Window(e.policy.ObservationWindow), e.memory, ev, present)
		rec.Containers = append(rec.Containers, cr)
	}
	return rec
}

// recommendResource is the single code path every strategy and every experiment
// goes through.
func (e *Engine) recommendResource(
	kind model.ResourceKind,
	current float64,
	series model.Series,
	strategy Strategy,
	ev model.RestartEvidence,
	evPresent bool,
) model.ResourceRecommendation {
	values := series.Values()
	summary := stats.Summarize(values, series.Duration(), e.policy.PercentileMethod)
	suff := e.sufficiency(series)

	out := model.ResourceRecommendation{
		Resource:        kind,
		Current:         current,
		Strategy:        strategy.ID(),
		SafetyFactor:    SafetyFactorOf(strategy),
		Stats:           toObservedStats(summary),
		DataSufficiency: suff,
	}

	raw := strategy.Target(summary)
	if raw == sentinelUseCurrent {
		// The "current" baseline: no statistical target at all.
		out.RawTarget = current
		out.Target = current
		out.Decision = model.DecisionNoChange
		out.Risk = model.RiskLow
		out.Reason = "baseline: current declared request retained without modification"
		return out
	}
	out.RawTarget = raw

	target := raw
	blocked := false
	var gateReasons []string
	var firedGates []string

	for _, g := range e.gates {
		res := g.Evaluate(GateInput{
			Resource:        kind,
			Current:         current,
			Target:          target,
			Summary:         summary,
			Evidence:        ev,
			EvidencePresent: evPresent,
			Sufficiency:     suff,
		})
		// A gate may never lower the target: safety mechanisms only ever hold
		// the line or raise it.
		if res.Target < target && !res.Block {
			panic(fmt.Sprintf("gate %s lowered target from %v to %v without blocking", g.ID(), target, res.Target))
		}
		if res.Fired {
			firedGates = append(firedGates, g.ID())
			if res.Reason != "" {
				gateReasons = append(gateReasons, res.Reason)
			}
		}
		target = res.Target
		if res.Block {
			blocked = true
			break
		}
	}

	if e.policy.Rounding {
		switch kind {
		case model.ResourceCPU:
			target = float64(quantity.RoundCPUUp(model.Millicores(target)))
		case model.ResourceMemory:
			target = float64(quantity.RoundMemoryUp(model.Bytes(target)))
		}
	}

	out.Target = target
	out.Gates = firedGates

	switch {
	case !suff.Sufficient:
		out.Decision = model.DecisionInsufficientData
		out.Target = 0
		out.Risk = model.RiskUnknown
		out.Reason = gateReasons[0]
	case blocked:
		out.Decision = model.DecisionBlocked
		out.Risk = e.riskOf(kind, current, summary, ev, evPresent)
		out.Reason = strings.Join(gateReasons, "; ")
	case math.Abs(target-current) < 1e-9:
		out.Decision = model.DecisionNoChange
		out.Risk = model.RiskLow
		out.Reason = e.noChangeReason(strategy, gateReasons)
	case target < current:
		out.Decision = model.DecisionDecrease
		out.Risk = e.riskOf(kind, target, summary, ev, evPresent)
		out.Reason = e.changeReason(kind, strategy, summary, target, current, gateReasons)
	default:
		out.Decision = model.DecisionIncrease
		// An increase reduces reliability risk by construction, so it is
		// reported as low risk regardless of the evidence that motivated it.
		out.Risk = model.RiskLow
		out.Reason = e.changeReason(kind, strategy, summary, target, current, gateReasons)
	}
	return out
}

func (e *Engine) noChangeReason(s Strategy, gateReasons []string) string {
	if len(gateReasons) > 0 {
		return strings.Join(gateReasons, "; ")
	}
	return fmt.Sprintf("no change: %s matches the current request", s.Describe())
}

func (e *Engine) changeReason(
	kind model.ResourceKind, s Strategy, summary stats.Summary,
	target, current float64, gateReasons []string,
) string {
	var b strings.Builder
	verb := "decrease"
	if target > current {
		verb = "increase"
	}
	if kind == model.ResourceCPU {
		fmt.Fprintf(&b, "%s CPU request from %s to %s based on %s (observed mean %s, p95 %s, max %s over %s)",
			verb,
			quantity.CPUString(model.Millicores(current)), quantity.CPUString(model.Millicores(target)),
			s.Describe(),
			quantity.CPUString(model.Millicores(summary.Mean)),
			quantity.CPUString(model.Millicores(summary.P95)),
			quantity.CPUString(model.Millicores(summary.Max)),
			summary.WindowDuration.Round(time.Minute))
	} else {
		fmt.Fprintf(&b, "%s memory request from %s to %s based on %s (observed mean %s, p95 %s, max %s over %s)",
			verb,
			quantity.MemoryString(model.Bytes(current)), quantity.MemoryString(model.Bytes(target)),
			s.Describe(),
			quantity.MemoryString(model.Bytes(summary.Mean)),
			quantity.MemoryString(model.Bytes(summary.P95)),
			quantity.MemoryString(model.Bytes(summary.Max)),
			summary.WindowDuration.Round(time.Minute))
	}
	if len(gateReasons) > 0 {
		fmt.Fprintf(&b, "; %s", strings.Join(gateReasons, "; "))
	}
	return b.String()
}

// riskOf classifies the reliability risk of operating at target.
//
// The classification is intentionally coarse and rule-based rather than a
// score: a numeric risk score would imply a calibration the system does not
// have. The rules differ by resource because the consequences differ.
func (e *Engine) riskOf(
	kind model.ResourceKind, target float64, summary stats.Summary,
	ev model.RestartEvidence, evPresent bool,
) model.RiskLevel {
	if summary.Samples == 0 {
		return model.RiskUnknown
	}
	headroom := 0.0
	if summary.Max > 0 {
		headroom = target / summary.Max
	}
	switch kind {
	case model.ResourceMemory:
		// For memory, sitting below the observed maximum means an observed
		// working set would not have fitted: that is a high-risk position
		// regardless of how rare the peak was, because the outcome is a kill.
		if !evPresent {
			return model.RiskUnknown
		}
		switch {
		case ev.OOMKills > 0:
			return model.RiskHigh
		case headroom < 1.0:
			return model.RiskHigh
		case headroom < 1.15:
			return model.RiskModerate
		default:
			return model.RiskLow
		}
	default:
		// For CPU, operating below the observed maximum is normal and expected:
		// the consequence is throttling, and the request is not a hard cap
		// unless a limit is also set. Risk therefore tracks how much of the
		// observed distribution sits above the target.
		switch {
		case headroom < 0.5 && summary.Burstiness > 3:
			return model.RiskHigh
		case summary.P99 > target:
			return model.RiskModerate
		case ev.CPUThrottledRatio > 0.05:
			return model.RiskModerate
		default:
			return model.RiskLow
		}
	}
}

// sufficiency evaluates whether a series can support a decision.
//
// Coverage compares the number of samples present against the number expected
// from the series step over the observed span. A series can span seven days and
// still be mostly gaps; coverage is what distinguishes the two cases.
func (e *Engine) sufficiency(s model.Series) model.DataSufficiency {
	d := model.DataSufficiency{
		Samples:     s.Len(),
		MinSamples:  e.policy.MinSamples,
		Observed:    s.Duration(),
		MinObserved: e.policy.MinDuration,
	}
	expected := 0.0
	if s.Step > 0 && s.Duration() > 0 {
		expected = s.Duration().Seconds()/s.Step.Seconds() + 1
	}
	switch {
	case expected > 0:
		d.Coverage = math.Min(1.0, float64(s.Len())/expected)
	case s.Len() > 0:
		// Step unknown: coverage cannot be assessed, so it is reported as
		// unknown-but-permissive (1.0) and the sample/duration checks carry the
		// decision. Recording this explicitly avoids a silent 0 that would look
		// like total data loss.
		d.Coverage = 1.0
	}
	d.Sufficient = s.Len() >= e.policy.MinSamples &&
		s.Duration() >= e.policy.MinDuration &&
		d.Coverage >= e.policy.MinCoverage
	return d
}

func toObservedStats(s stats.Summary) model.ObservedStats {
	return model.ObservedStats{
		Samples: s.Samples, Mean: s.Mean, StdDev: s.StdDev, Min: s.Min,
		P50: s.P50, P90: s.P90, P95: s.P95, P99: s.P99, Max: s.Max,
		Burstiness: s.Burstiness, WindowDuration: s.WindowDuration,
	}
}
