package experiment

import (
	"fmt"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/simulator"
	"github.com/anishc23/k8s-cost-optimizer/internal/stats"
	"github.com/anishc23/k8s-cost-optimizer/pkg/humanize"
)

// Config defines an experiment: the matrix of conditions to evaluate and the
// parameters held fixed across them.
//
// YAML keys come from the json tags. sigs.k8s.io/yaml converts YAML to JSON and
// then uses encoding/json, so a `yaml:` tag on these fields would be inert — and
// an inert tag that looks authoritative is worse than none, because it silently
// makes a config field unsettable. Every key in experiments/configs/ is therefore
// the snake_case json name, and TestShippedConfigsLoad verifies that each shipped
// file actually populates the fields it appears to set.
//
// The struct is the unit of version control for experiments. Every file in
// experiments/configs/ deserialises into one of these, and a result record
// carries the config's hash, so a result can always be traced to the exact
// specification that produced it.
type Config struct {
	Name        string `json:"name"`
	Description string `json:"description"`

	// --- Matrix dimensions ---

	// Workloads selects the classes to evaluate. Empty means all.
	Workloads []simulator.Class `json:"workloads"`
	// CPUStrategies and MemoryStrategies are evaluated independently rather than
	// as a cross product of one shared list, because the central hypothesis is
	// that the best CPU policy and the best memory policy differ. Sharing one
	// list would make that hypothesis unexpressible.
	CPUStrategies    []string `json:"cpu_strategies"`
	MemoryStrategies []string `json:"memory_strategies"`
	// ObservationWindows are the history lengths to evaluate (RQ6).
	ObservationWindows humanize.Durations `json:"observation_windows"`
	// SafetyFactors are applied to both resources unless MemorySafetyFactors is
	// set, in which case the two vary independently.
	SafetyFactors       []float64 `json:"safety_factors"`
	MemorySafetyFactors []float64 `json:"memory_safety_factors,omitempty"`

	// Seeds is the number of independent trace realisations per condition.
	//
	// Repetition is what makes the results more than anecdote: a single trace of
	// a stochastic workload could favour any policy by chance. Each seed is a
	// fresh draw from the same generative model, so variation across seeds
	// measures how much of a reported difference is sampling noise.
	Seeds int `json:"seeds"`
	// BaseSeed anchors the seed sequence so runs are reproducible.
	BaseSeed int64 `json:"base_seed"`

	// --- Fixed parameters ---

	// TraceDuration is how much demand to generate. It must be at least as long
	// as the longest observation window, plus the evaluation horizon.
	TraceDuration humanize.Duration `json:"trace_duration"`
	// Step is the sampling resolution.
	Step humanize.Duration `json:"step"`

	// EvaluationHorizon is the portion of the trace held out for scoring.
	//
	// This is the most important methodological choice in the config. The engine
	// fits its recommendation to the observation window; the recommendation is
	// then scored against the *following* period, which the engine never saw.
	// Scoring on the fitting window would measure how well a percentile describes
	// the data it was computed from — which is a tautology, not a result. Holding
	// out the horizon makes the evaluation a genuine prediction task: will this
	// request size hold up over the next N hours?
	EvaluationHorizon humanize.Duration `json:"evaluation_horizon"`

	PercentileMethod stats.PercentileMethod `json:"percentile_method"`

	// Gates that may be disabled for ablation studies.
	DisableOOMProtection bool `json:"disable_oom_protection"`
	DisableUsageExceeds  bool `json:"disable_usage_exceeds"`
	DisableFloors        bool `json:"disable_floors"`
	DisableMinChange     bool `json:"disable_min_change"`
	DisableRounding      bool `json:"disable_rounding"`
	// UnifiedStrategy forces the memory strategy to equal the CPU strategy, for
	// the ablation that treats the two resources identically (Ablation C).
	UnifiedStrategy bool `json:"unified_strategy"`

	// Feasibility is the reliability constraint used to compute FeasibleSavings.
	Feasibility FeasibilityConstraint `json:"feasibility"`

	// CostInstance names the instance type whose price derives the cost model.
	CostInstance string `json:"cost_instance"`
	// CPUCostShare is the CPU/memory price split; zero uses the package default.
	CPUCostShare float64 `json:"cpu_cost_share"`

	// Replay configures the ground-truth evaluation semantics.
	CPURequestIsCeiling      *bool `json:"cpu_request_is_ceiling,omitempty"`
	MemoryLimitEqualsRequest *bool `json:"memory_limit_equals_request,omitempty"`

	// StabilityRecomputations, when > 1, recomputes each recommendation over
	// that many successive windows to measure volatility (RQ7).
	StabilityRecomputations int `json:"stability_recomputations"`
}

// Validate checks a configuration for internal consistency before any work is
// done, so that a long run fails in the first second rather than the last.
func (c *Config) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("experiment name is required")
	}
	if len(c.CPUStrategies) == 0 || len(c.MemoryStrategies) == 0 {
		return fmt.Errorf("%s: at least one CPU and one memory strategy are required", c.Name)
	}
	if len(c.ObservationWindows) == 0 {
		return fmt.Errorf("%s: at least one observation window is required", c.Name)
	}
	if len(c.SafetyFactors) == 0 {
		return fmt.Errorf("%s: at least one safety factor is required", c.Name)
	}
	for _, sf := range append(append([]float64{}, c.SafetyFactors...), c.MemorySafetyFactors...) {
		if sf < 1.0 {
			return fmt.Errorf("%s: safety factor %.2f is below 1.0", c.Name, sf)
		}
	}
	if c.Seeds < 1 {
		return fmt.Errorf("%s: seeds must be at least 1", c.Name)
	}
	if c.Step.D() <= 0 {
		return fmt.Errorf("%s: step must be positive", c.Name)
	}
	if c.EvaluationHorizon.D() <= 0 {
		return fmt.Errorf("%s: evaluation_horizon must be positive; scoring on the fitting window is not a valid evaluation", c.Name)
	}
	longest := time.Duration(0)
	for _, w := range c.ObservationWindows.Std() {
		if w <= 0 {
			return fmt.Errorf("%s: observation window %s must be positive", c.Name, w)
		}
		if w > longest {
			longest = w
		}
	}
	// The trace must hold the longest fitting window plus the held-out horizon,
	// otherwise the longest-window condition would silently be fitted on less
	// data than requested and RQ6 would compare windows that do not exist.
	if need := longest + c.EvaluationHorizon.D(); c.TraceDuration.D() < need {
		return fmt.Errorf(
			"%s: trace_duration %s is shorter than the longest observation window plus the evaluation horizon (%s); "+
				"the longest-window condition would be fitted on truncated data",
			c.Name, c.TraceDuration, need)
	}
	if c.StabilityRecomputations > 0 {
		need := longest + time.Duration(c.StabilityRecomputations)*c.EvaluationHorizon.D()
		if c.TraceDuration.D() < need {
			return fmt.Errorf("%s: trace_duration %s is too short for %d stability recomputations (need %s)",
				c.Name, c.TraceDuration, c.StabilityRecomputations, need)
		}
	}
	return nil
}

// Conditions returns the number of (workload, cpu strategy, memory strategy,
// window, safety factor, seed) combinations this config expands to. It is
// reported before a run so the operator knows the scale of what they started.
func (c *Config) Conditions() int {
	w := len(c.Workloads)
	if w == 0 {
		w = len(simulator.AllClasses())
	}
	mem := len(c.MemoryStrategies)
	if c.UnifiedStrategy {
		mem = 1
	}
	memSF := len(c.MemorySafetyFactors)
	if memSF == 0 {
		memSF = 1 // memory reuses the CPU safety factor
	}
	return w * len(c.CPUStrategies) * mem * len(c.ObservationWindows) *
		len(c.SafetyFactors) * memSF * c.Seeds
}

// replayConfig resolves the replay semantics, applying defaults for unset
// pointers.
func (c *Config) replayConfig() simulator.Config {
	cfg := simulator.DefaultConfig()
	if c.CPURequestIsCeiling != nil {
		cfg.CPURequestIsCeiling = *c.CPURequestIsCeiling
	}
	if c.MemoryLimitEqualsRequest != nil {
		cfg.MemoryLimitEqualsRequest = *c.MemoryLimitEqualsRequest
	}
	return cfg
}

func (c *Config) classes() []simulator.Class {
	if len(c.Workloads) > 0 {
		return c.Workloads
	}
	return simulator.AllClasses()
}

func (c *Config) memorySafetyFactors() []float64 {
	if len(c.MemorySafetyFactors) > 0 {
		return c.MemorySafetyFactors
	}
	return nil // signals "mirror the CPU factor"
}
