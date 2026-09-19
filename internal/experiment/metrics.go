// Package experiment runs the controlled studies described in
// research/methodology.md and writes their results as machine-readable records.
//
// The package is deliberately thin. It expands a configuration into a matrix of
// conditions, invokes the *production* recommendation engine on each, replays
// the resulting configuration against ground-truth demand, and records both. It
// contains no right-sizing logic of its own: if it did, the experiments would be
// measuring code that is not deployed.
package experiment

import (
	"math"

	"github.com/anishc23/k8s-cost-optimizer/internal/simulator"
)

// Metrics are the evaluation measures reported for one experimental condition.
//
// Every metric here is defined in research/methodology.md with its formula and
// its intended interpretation. Two properties were required of each:
//
//   - It must be computable from ground truth, not from the censored series the
//     engine observed.
//   - It must be scale-free where it will be pooled across workloads of
//     different sizes, so that averaging is meaningful rather than dominated by
//     the largest workload.
//
// Metrics that failed the second test are reported both in absolute and
// normalised form (throttled core-seconds alongside throttle fraction).
type Metrics struct {
	// --- Cost ---

	// SavingsFraction is (current cost - recommended cost) / current cost.
	// Negative when the engine recommends an increase, which is reported rather
	// than clipped.
	SavingsFraction float64 `json:"savings_fraction"`
	// MonthlySavingsUSD is the absolute allocation-based saving.
	MonthlySavingsUSD float64 `json:"monthly_savings_usd"`
	// CurrentMonthlyUSD anchors the fraction, so that a large percentage on a
	// trivial workload is distinguishable from the same percentage on a large one.
	CurrentMonthlyUSD float64 `json:"current_monthly_usd"`

	// --- Efficiency ---

	// CPUUtilization is mean served CPU demand divided by the CPU request.
	// It is the efficiency term of the trade-off: higher means less reserved
	// capacity sitting idle.
	CPUUtilization float64 `json:"cpu_utilization"`
	// MemoryUtilization is mean working-set demand divided by the memory request.
	MemoryUtilization float64 `json:"memory_utilization"`

	// --- Risk ---

	// CPUViolationRate is the fraction of samples whose demand exceeded the CPU
	// request: how *often* the workload was degraded.
	CPUViolationRate float64 `json:"cpu_violation_rate"`
	// CPUThrottleFraction is unmet CPU work divided by demanded CPU work: how
	// *much* was degraded. Frequency and magnitude are reported separately
	// because a policy can be good at one and bad at the other.
	CPUThrottleFraction float64 `json:"cpu_throttle_fraction"`
	// CPUThrottledCoreSeconds is the unnormalised magnitude.
	CPUThrottledCoreSeconds float64 `json:"cpu_throttled_core_seconds"`
	// MemoryViolationRate is the fraction of samples above the memory limit.
	MemoryViolationRate float64 `json:"memory_violation_rate"`
	// OOMKills is the count of distinct kill episodes under the replay model.
	OOMKills int `json:"oom_kills"`
	// OOMKillsPerDay normalises that count by trace length.
	OOMKillsPerDay float64 `json:"oom_kills_per_day"`
	// SurvivedWithoutOOM is the headline reliability outcome for memory: a
	// binary is more interpretable than a rate when the failure is fatal, and it
	// is what an operator actually cares about.
	SurvivedWithoutOOM bool `json:"survived_without_oom"`

	// --- Provisioning quality relative to ground truth ---

	// CPUOverProvisionRatio is request / true max demand. Exactly 1.0 is the
	// tightest configuration that never degrades; above 1 is headroom paid for,
	// below 1 is degradation accepted.
	CPUOverProvisionRatio float64 `json:"cpu_over_provision_ratio"`
	// MemoryOverProvisionRatio is the same for memory. Below 1.0 means the
	// configuration could not hold an observed working set.
	MemoryOverProvisionRatio float64 `json:"memory_over_provision_ratio"`

	// CPURelativeError is (request - true p99 demand) / true p99 demand, a
	// signed accuracy measure against a defensible operating target. p99 is used
	// as the reference rather than the maximum because sizing every workload to
	// its single largest sample is not the operating point most teams choose,
	// and an error metric should be measured against a realistic target.
	CPURelativeError float64 `json:"cpu_relative_error"`
	// MemoryRelativeError is measured against true *maximum* demand, not p99,
	// because for memory the maximum is the operating target: anything below it
	// is a kill.
	MemoryRelativeError float64 `json:"memory_relative_error"`

	// --- Composite ---

	// FeasibleSavings is SavingsFraction when the condition met the reliability
	// constraint, and 0 otherwise.
	//
	// This is a constrained objective, not a weighted score. A weighted score
	// (savings minus lambda times risk) would require choosing lambda, which
	// encodes a value judgement about how many OOMKills a dollar is worth, and
	// would let a policy trade reliability for savings at a rate no operator
	// agreed to. The constrained form instead answers the question operators
	// actually ask: among configurations that are acceptably safe, which saves
	// most? The constraint is stated explicitly in FeasibilityConstraint.
	FeasibleSavings float64 `json:"feasible_savings"`
	// Feasible records whether the reliability constraint was met.
	Feasible bool `json:"feasible"`
}

// FeasibilityConstraint defines what counts as an acceptably safe configuration.
//
// The thresholds are parameters of the analysis rather than universal truths,
// and the sensitivity analysis varies them. The defaults encode a specific and
// defensible operating stance:
//
//	MaxOOMKills = 0        — a memory configuration that kills the workload
//	                         even once has failed, regardless of savings.
//	MaxCPUThrottleFraction — some CPU degradation is acceptable; 1% of demanded
//	                         work unserved is a normal operating point for a
//	                         service with headroom elsewhere.
//
// The asymmetry between the two is the point: it is the CPU/memory distinction
// expressed as an evaluation criterion rather than only as an algorithm.
type FeasibilityConstraint struct {
	MaxOOMKills            int     `json:"max_oom_kills"`
	MaxCPUThrottleFraction float64 `json:"max_cpu_throttle_fraction"`
}

// DefaultFeasibility is the constraint used for the headline results.
func DefaultFeasibility() FeasibilityConstraint {
	return FeasibilityConstraint{MaxOOMKills: 0, MaxCPUThrottleFraction: 0.01}
}

// Satisfied reports whether an outcome meets the constraint.
func (c FeasibilityConstraint) Satisfied(o simulator.Outcome) bool {
	return o.OOMKills <= c.MaxOOMKills && o.CPUThrottleFraction <= c.MaxCPUThrottleFraction
}

// ComputeMetrics derives the evaluation measures for one condition.
//
// currentMonthly and recommendedMonthly come from the cost estimator so that
// experimental savings and production savings are computed by identical code.
func ComputeMetrics(
	tr simulator.Trace,
	out simulator.Outcome,
	currentMonthly, recommendedMonthly float64,
	constraint FeasibilityConstraint,
) Metrics {
	m := Metrics{
		MonthlySavingsUSD:        currentMonthly - recommendedMonthly,
		CurrentMonthlyUSD:        currentMonthly,
		SavingsFraction:          simulator.SavingsFraction(currentMonthly, recommendedMonthly),
		CPUUtilization:           out.CPUUtilization,
		MemoryUtilization:        out.MemoryUtilization,
		CPUViolationRate:         out.CPUViolationRate,
		CPUThrottleFraction:      out.CPUThrottleFraction,
		CPUThrottledCoreSeconds:  out.CPUThrottledCoreSeconds,
		MemoryViolationRate:      out.MemoryViolationRate,
		OOMKills:                 out.OOMKills,
		OOMKillsPerDay:           out.OOMKillsPerDay,
		SurvivedWithoutOOM:       out.OOMKills == 0,
		CPUOverProvisionRatio:    out.CPUOverProvisionRatio,
		MemoryOverProvisionRatio: out.MemoryOverProvisionRatio,
	}
	if gt := tr.GroundTruth.CPUP99Milli; gt > 0 {
		m.CPURelativeError = (out.CPURequestMilli - gt) / gt
	}
	if gt := tr.GroundTruth.MemoryMaxBytes; gt > 0 {
		m.MemoryRelativeError = (out.MemoryRequestBytes - gt) / gt
	}
	m.Feasible = constraint.Satisfied(out)
	if m.Feasible {
		m.FeasibleSavings = m.SavingsFraction
	}
	return m
}

// StabilityMetrics quantify how much a recommendation moves when it is
// recomputed over successive observation windows.
//
// Volatility matters operationally: applying a request change restarts every pod
// in the workload, so a recommender whose output swings between runs is not
// deployable however accurate each individual recommendation is. This is a
// systems property that accuracy metrics alone do not capture, which is why RQ7
// treats it as a first-class result rather than a footnote.
type StabilityMetrics struct {
	// Recomputations is the number of successive recommendations compared.
	Recomputations int `json:"recomputations"`
	// MedianAbsLog2Ratio is the median of |log2(r_t / r_{t-1})| across
	// consecutive recommendations.
	//
	// The log-ratio is used rather than a relative difference because it is
	// symmetric: a doubling and a halving are the same magnitude of change, which
	// a percentage difference gets wrong (+100% versus -50%). A value of 0 means
	// perfectly stable; 1.0 means the typical recomputation doubled or halved the
	// request.
	MedianAbsLog2Ratio float64 `json:"median_abs_log2_ratio"`
	// MaxAbsLog2Ratio is the worst single swing.
	MaxAbsLog2Ratio float64 `json:"max_abs_log2_ratio"`
	// ChangeRate is the fraction of recomputations that would have triggered a
	// rollout under the policy's minimum-change threshold: the operationally
	// meaningful churn rate.
	ChangeRate float64 `json:"change_rate"`
	// RangeRatio is max(r)/min(r) over all recomputations, describing the total
	// spread an operator would have seen.
	RangeRatio float64 `json:"range_ratio"`
}

// ComputeStability summarises a sequence of recommendations for one resource.
//
// Values of zero are skipped rather than treated as a change to zero: a zero
// arises from INSUFFICIENT_DATA, which is an absence of a recommendation, not a
// recommendation of nothing. Counting it as a swing to zero would report enormous
// volatility for a recommender that simply declined to answer.
func ComputeStability(values []float64, minRelativeChange float64) StabilityMetrics {
	var s StabilityMetrics
	usable := make([]float64, 0, len(values))
	for _, v := range values {
		if v > 0 {
			usable = append(usable, v)
		}
	}
	if len(usable) < 2 {
		return s
	}
	s.Recomputations = len(usable) - 1
	ratios := make([]float64, 0, len(usable)-1)
	changes := 0
	minV, maxV := usable[0], usable[0]
	for i := 1; i < len(usable); i++ {
		prev, cur := usable[i-1], usable[i]
		r := math.Abs(math.Log2(cur / prev))
		ratios = append(ratios, r)
		if r > s.MaxAbsLog2Ratio {
			s.MaxAbsLog2Ratio = r
		}
		if math.Abs(cur-prev)/prev >= minRelativeChange {
			changes++
		}
		if cur < minV {
			minV = cur
		}
		if cur > maxV {
			maxV = cur
		}
	}
	s.MedianAbsLog2Ratio = median(ratios)
	s.ChangeRate = float64(changes) / float64(len(ratios))
	if minV > 0 {
		s.RangeRatio = maxV / minV
	}
	return s
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	quickSort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func quickSort(v []float64) {
	if len(v) < 2 {
		return
	}
	// Insertion sort: the slices here are at most a few dozen elements (one per
	// recomputation), where it beats the overhead of anything cleverer.
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
