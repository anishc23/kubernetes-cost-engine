package model

import "time"

// Decision is the outcome of evaluating one (container, resource) pair.
//
// NO_CHANGE and BLOCKED are distinct on purpose. NO_CHANGE means the engine
// computed a recommendation and it was not materially different from the
// current request. BLOCKED means a safety gate refused to emit a reduction that
// the raw statistics would otherwise have supported. Collapsing the two would
// hide exactly the cases an operator most needs to see.
type Decision string

const (
	DecisionDecrease Decision = "DECREASE"
	DecisionIncrease Decision = "INCREASE"
	DecisionNoChange Decision = "NO_CHANGE"
	DecisionBlocked  Decision = "BLOCKED"
	// DecisionInsufficientData means the observation window did not contain
	// enough samples to make any claim. It never carries a target value.
	DecisionInsufficientData Decision = "INSUFFICIENT_DATA"
)

// RiskLevel is the engine's qualitative assessment of applying a recommendation.
type RiskLevel string

const (
	RiskLow      RiskLevel = "LOW"
	RiskModerate RiskLevel = "MODERATE"
	RiskHigh     RiskLevel = "HIGH"
	RiskUnknown  RiskLevel = "UNKNOWN"
)

// ResourceRecommendation is the engine's advice for a single resource of a
// single container, together with the evidence behind it.
//
// Every field that a reader would need in order to disagree with the engine is
// included. This is a deliberate design constraint: an advisory system whose
// reasoning cannot be audited will not be trusted in production, and an
// experiment whose inputs are not recorded cannot be reproduced.
type ResourceRecommendation struct {
	Resource ResourceKind `json:"resource"`
	Decision Decision     `json:"decision"`
	Risk     RiskLevel    `json:"risk"`

	// Current is the declared request, in canonical units.
	Current float64 `json:"current"`
	// Target is the recommended request, in canonical units. It is zero when
	// Decision is INSUFFICIENT_DATA.
	Target float64 `json:"target"`
	// RawTarget is the value the statistical policy produced before rounding
	// and before safety gates were applied. Reported so that the effect of the
	// gates is measurable rather than invisible.
	RawTarget float64 `json:"raw_target"`

	// Strategy is the identifier of the policy that produced RawTarget.
	Strategy string `json:"strategy"`
	// SafetyFactor is the multiplier applied to the statistic.
	SafetyFactor float64 `json:"safety_factor"`

	// Statistics of the observed series over the window, in canonical units.
	Stats ObservedStats `json:"stats"`

	// Reason is a single human-readable sentence explaining the decision.
	Reason string `json:"reason"`
	// Gates lists the safety gates that fired, if any.
	Gates []string `json:"gates,omitempty"`

	// Confidence reflects data sufficiency, not statistical confidence in a
	// parameter estimate. Named explicitly to avoid implying the latter.
	DataSufficiency DataSufficiency `json:"data_sufficiency"`
}

// ObservedStats is the summary of an observed usage series. All values are in
// canonical units (millicores or bytes).
type ObservedStats struct {
	Samples int     `json:"samples"`
	Mean    float64 `json:"mean"`
	StdDev  float64 `json:"stddev"`
	Min     float64 `json:"min"`
	P50     float64 `json:"p50"`
	P90     float64 `json:"p90"`
	P95     float64 `json:"p95"`
	P99     float64 `json:"p99"`
	Max     float64 `json:"max"`
	// Burstiness is max/mean, a scale-free indicator of peak-to-average ratio.
	// It is reported because it is the single statistic that best predicts
	// whether a percentile policy will under-provision (see research/results.md).
	Burstiness float64 `json:"burstiness"`
	// WindowDuration is the span the statistics were computed over.
	WindowDuration time.Duration `json:"window_duration"`
}

// DataSufficiency records whether enough data existed to support a decision.
type DataSufficiency struct {
	Sufficient  bool          `json:"sufficient"`
	Samples     int           `json:"samples"`
	MinSamples  int           `json:"min_samples"`
	Observed    time.Duration `json:"observed_window"`
	MinObserved time.Duration `json:"min_observed_window"`
	Coverage    float64       `json:"coverage"`
}

// ContainerRecommendation pairs the CPU and memory advice for one container.
type ContainerRecommendation struct {
	Container string                 `json:"container"`
	CPU       ResourceRecommendation `json:"cpu"`
	Memory    ResourceRecommendation `json:"memory"`
}

// WorkloadRecommendation is the engine's complete output for one workload.
type WorkloadRecommendation struct {
	Namespace   string                    `json:"namespace"`
	Name        string                    `json:"name"`
	Kind        WorkloadKind              `json:"kind"`
	Replicas    int32                     `json:"replicas"`
	GeneratedAt time.Time                 `json:"generated_at"`
	Containers  []ContainerRecommendation `json:"containers"`

	// Cost holds the allocation-based cost estimate for the current and
	// recommended configurations. See internal/cost for its stated limits.
	Cost CostEstimate `json:"cost"`

	// PolicyID identifies the full policy configuration used, so that a
	// recommendation can be traced back to an exact engine configuration.
	PolicyID string `json:"policy_id"`
}

// Key is the stable identifier for the workload this recommendation describes.
func (r WorkloadRecommendation) Key() string { return r.Namespace + "/" + r.Name }

// CostEstimate is an allocation-based cost estimate: it prices reserved
// capacity, not cloud invoices. See docs/cost-model.md for what this does and
// does not capture.
type CostEstimate struct {
	CurrentMonthlyUSD     float64 `json:"current_monthly_usd"`
	RecommendedMonthlyUSD float64 `json:"recommended_monthly_usd"`
	MonthlySavingsUSD     float64 `json:"monthly_savings_usd"`
	SavingsFraction       float64 `json:"savings_fraction"`
	Model                 string  `json:"model"`
}
