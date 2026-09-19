// Package recommender implements the right-sizing policies the project
// evaluates, and the safety gates applied on top of them.
//
// The package is structured around a deliberate separation:
//
//	Strategy  — a pure statistical function from an observed series to a target.
//	Gate      — a safety rule that can veto or raise a target using evidence
//	            outside the series (OOM history, data sufficiency, throttling).
//	Engine    — composes a strategy per resource with the gates.
//
// Keeping strategies pure is what makes the experimental comparison meaningful:
// every baseline in research/methodology.md is the same code path with a
// different Strategy, so a difference in results cannot come from a difference
// in plumbing.
package recommender

import (
	"fmt"
	"sort"

	"github.com/anishc23/k8s-cost-optimizer/internal/stats"
)

// Strategy maps a statistical summary of observed usage to a target request, in
// the same canonical units as the input (millicores or bytes).
//
// A Strategy must not consult reliability evidence or the current request: those
// belong to the gates. This restriction is what keeps the baselines honest — a
// "strategy" that peeked at the current request could trivially never regress.
type Strategy interface {
	// ID is the stable identifier recorded in experiment results.
	ID() string
	// Target returns the raw recommended value from the summary.
	Target(s stats.Summary) float64
	// Describe returns a one-line explanation used in recommendation reasons.
	Describe() string
}

// percentileStrategy implements the percentile family, optionally scaled by a
// safety factor. All of the CPU and memory baselines in the study are instances
// of this type, which guarantees they differ only in their parameters.
type percentileStrategy struct {
	id           string
	percentile   float64 // in [0,1]; 1.0 means the observed maximum
	safetyFactor float64
	method       stats.PercentileMethod
}

func (p percentileStrategy) ID() string { return p.id }

func (p percentileStrategy) Target(s stats.Summary) float64 {
	return p.statistic(s) * p.safetyFactor
}

func (p percentileStrategy) statistic(s stats.Summary) float64 {
	// The common percentiles are precomputed in the summary; anything else is
	// not offered, because recomputing here would require the raw series and
	// would let a strategy use a different estimator than the one recorded.
	switch p.percentile {
	case 0.5:
		return s.P50
	case 0.9:
		return s.P90
	case 0.95:
		return s.P95
	case 0.99:
		return s.P99
	case 1.0:
		return s.Max
	default:
		panic(fmt.Sprintf("percentileStrategy %q: unsupported percentile %v", p.id, p.percentile))
	}
}

func (p percentileStrategy) Describe() string {
	name := "max"
	if p.percentile < 1.0 {
		name = fmt.Sprintf("p%g", p.percentile*100)
	}
	if p.safetyFactor == 1.0 {
		return fmt.Sprintf("%s of observed usage", name)
	}
	return fmt.Sprintf("%s of observed usage x %.2f safety factor", name, p.safetyFactor)
}

// meanStrategy recommends the mean observed usage, scaled by a safety factor.
//
// It is included as a baseline precisely because it is expected to perform
// badly on bursty workloads: a baseline that is never chosen is still needed to
// quantify how much the percentile choice is worth.
type meanStrategy struct {
	id           string
	safetyFactor float64
}

func (m meanStrategy) ID() string                     { return m.id }
func (m meanStrategy) Target(s stats.Summary) float64 { return s.Mean * m.safetyFactor }
func (m meanStrategy) Describe() string {
	if m.safetyFactor == 1.0 {
		return "mean observed usage"
	}
	return fmt.Sprintf("mean observed usage x %.2f safety factor", m.safetyFactor)
}

// currentStrategy is Baseline A: leave the request unchanged.
//
// It reports a sentinel of -1 because its target is not a function of the
// series at all; the Engine substitutes the current request. Modelling
// "do nothing" as a strategy rather than a special case means the experiment
// framework measures it with identical machinery, including its cost and its
// (zero) violation rate.
type currentStrategy struct{}

func (currentStrategy) ID() string                   { return "current" }
func (currentStrategy) Target(stats.Summary) float64 { return sentinelUseCurrent }
func (currentStrategy) Describe() string             { return "current declared request (unchanged)" }

const sentinelUseCurrent = -1.0

// NewPercentile builds a percentile strategy. percentile is in [0,1] and must
// be one of the precomputed quantiles (0.5, 0.9, 0.95, 0.99) or 1.0 for max.
func NewPercentile(id string, percentile, safetyFactor float64, method stats.PercentileMethod) Strategy {
	return percentileStrategy{id: id, percentile: percentile, safetyFactor: safetyFactor, method: method}
}

// NewMean builds a mean-based strategy.
func NewMean(id string, safetyFactor float64) Strategy {
	return meanStrategy{id: id, safetyFactor: safetyFactor}
}

// NewCurrent builds the unchanged-request baseline.
func NewCurrent() Strategy { return currentStrategy{} }

// Registry maps strategy IDs to constructors parameterised by safety factor.
//
// The experiment matrix is expanded from this registry, so adding a strategy
// here is sufficient to include it in every experiment, ablation and figure.
type Registry struct {
	method stats.PercentileMethod
	build  map[string]func(safetyFactor float64) Strategy
}

// NewRegistry returns the registry of all strategies evaluated in the study.
//
// Naming convention: the ID describes the statistic only. The safety factor is
// recorded as a separate experiment dimension rather than baked into the name,
// because "p95 at margin 1.0" and "p95 at margin 1.3" must be comparable rows
// in the same analysis rather than two unrelated strategies.
func NewRegistry(method stats.PercentileMethod) *Registry {
	r := &Registry{method: method, build: map[string]func(float64) Strategy{}}
	r.build["current"] = func(float64) Strategy { return NewCurrent() }
	r.build["mean"] = func(sf float64) Strategy { return NewMean("mean", sf) }
	for id, p := range map[string]float64{
		"p50": 0.5, "p90": 0.9, "p95": 0.95, "p99": 0.99, "max": 1.0,
	} {
		id, p := id, p
		r.build[id] = func(sf float64) Strategy { return NewPercentile(id, p, sf, method) }
	}
	return r
}

// Get returns the strategy with the given ID at the given safety factor.
func (r *Registry) Get(id string, safetyFactor float64) (Strategy, error) {
	b, ok := r.build[id]
	if !ok {
		return nil, fmt.Errorf("unknown strategy %q (known: %v)", id, r.IDs())
	}
	return b(safetyFactor), nil
}

// MustGet is Get for configuration already validated at load time.
func (r *Registry) MustGet(id string, safetyFactor float64) Strategy {
	s, err := r.Get(id, safetyFactor)
	if err != nil {
		panic(err)
	}
	return s
}

// IDs returns the known strategy IDs in deterministic order, which matters
// because it fixes the row order of generated experiment files.
func (r *Registry) IDs() []string {
	ids := make([]string, 0, len(r.build))
	for id := range r.build {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// SafetyFactorOf reports the safety factor a strategy was built with, for
// recording in results. Strategies whose target does not scale with a factor
// report 1.0.
func SafetyFactorOf(s Strategy) float64 {
	switch v := s.(type) {
	case percentileStrategy:
		return v.safetyFactor
	case meanStrategy:
		return v.safetyFactor
	default:
		return 1.0
	}
}
