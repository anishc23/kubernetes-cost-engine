// Package stats provides the numerical primitives used by the recommendation
// engine and the experiment analysis.
//
// Percentile estimation is given more care here than is usual in cost tooling,
// because the choice of estimator materially changes recommendations on short
// observation windows. A p99 computed from 60 samples (one hour at one-minute
// resolution) interpolates between the two largest observations; whether it
// returns the maximum or something below it is an estimator decision, not a
// property of the workload. Making that decision explicit is a prerequisite for
// the observation-window study in RQ6.
package stats

import (
	"math"
	"sort"
	"time"
)

// PercentileMethod selects the quantile estimator.
type PercentileMethod string

const (
	// LinearInterpolation is the R-7 / numpy default estimator: for n sorted
	// samples, the p-quantile sits at index (n-1)*p, interpolating between
	// neighbours. Chosen as the engine default because it is the estimator a
	// reader will reproduce with numpy.percentile, which matters for a project
	// whose analysis pipeline is Python.
	LinearInterpolation PercentileMethod = "linear"
	// NearestRank is the classical "smallest observation at or above the
	// p-fraction" estimator. It never returns a value between two samples, so
	// a p99 over a short window will return an actually-observed value. Offered
	// because for safety-critical sizing, an interpolated value that was never
	// observed is harder to defend.
	NearestRank PercentileMethod = "nearest-rank"
)

// Percentile returns the p-quantile (p in [0,1]) of values using the given
// method. It does not mutate the input. Returns 0 for an empty input, because
// the callers treat an empty series as insufficient data before reaching here.
func Percentile(values []float64, p float64, method PercentileMethod) float64 {
	if len(values) == 0 {
		return 0
	}
	if p <= 0 {
		return Min(values)
	}
	if p >= 1 {
		return Max(values)
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	return percentileSorted(sorted, p, method)
}

func percentileSorted(sorted []float64, p float64, method PercentileMethod) float64 {
	n := len(sorted)
	switch method {
	case NearestRank:
		// Smallest observation whose rank is at least ceil(p*n).
		rank := int(math.Ceil(p * float64(n)))
		if rank < 1 {
			rank = 1
		}
		if rank > n {
			rank = n
		}
		return sorted[rank-1]
	default: // LinearInterpolation
		pos := p * float64(n-1)
		lo := int(math.Floor(pos))
		hi := int(math.Ceil(pos))
		if lo == hi {
			return sorted[lo]
		}
		frac := pos - float64(lo)
		return sorted[lo]*(1-frac) + sorted[hi]*frac
	}
}

// Percentiles computes several quantiles in one pass over a single sort, which
// matters because the engine always needs p50/p90/p95/p99 together.
func Percentiles(values []float64, ps []float64, method PercentileMethod) []float64 {
	out := make([]float64, len(ps))
	if len(values) == 0 {
		return out
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	for i, p := range ps {
		switch {
		case p <= 0:
			out[i] = sorted[0]
		case p >= 1:
			out[i] = sorted[len(sorted)-1]
		default:
			out[i] = percentileSorted(sorted, p, method)
		}
	}
	return out
}

// Mean returns the arithmetic mean, or 0 for an empty input.
func Mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	// Kahan summation: usage series can be long (7 days at 30s resolution is
	// ~20k samples) and memory values are large (bytes), so naive summation
	// loses precision in a way that shifts the mean.
	var sum, c float64
	for _, v := range values {
		y := v - c
		t := sum + y
		c = (t - sum) - y
		sum = t
	}
	return sum / float64(len(values))
}

// StdDev returns the sample standard deviation (Bessel-corrected, n-1). The
// sample form is used because a usage series is a sample of the workload's
// behaviour, not the whole population of its possible behaviour.
func StdDev(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	m := Mean(values)
	var ss float64
	for _, v := range values {
		d := v - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(values)-1))
}

// Min returns the smallest value, or 0 for an empty input.
func Min(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	m := values[0]
	for _, v := range values[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

// Max returns the largest value, or 0 for an empty input.
func Max(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	m := values[0]
	for _, v := range values[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

// CoefficientOfVariation is StdDev/Mean, a scale-free dispersion measure. It
// returns 0 when the mean is zero, since dispersion relative to nothing is not
// defined.
func CoefficientOfVariation(values []float64) float64 {
	m := Mean(values)
	if m == 0 {
		return 0
	}
	return StdDev(values) / m
}

// Median is a convenience wrapper used by the analysis-facing code.
func Median(values []float64) float64 { return Percentile(values, 0.5, LinearInterpolation) }

// MeanCI returns the mean and the half-width of a normal-approximation
// confidence interval at the given two-sided level.
//
// The normal approximation is used rather than a t-distribution because the
// experiment framework aggregates over >= 10 seeds and the analysis layer
// (analysis/scripts) recomputes intervals with scipy where the distributional
// assumption is examined properly. Callers should treat this as an indicative
// interval for operator-facing display, not as an inferential result; the
// reported research intervals are bootstrap intervals computed in Python.
func MeanCI(values []float64, z float64) (mean, halfWidth float64) {
	mean = Mean(values)
	if len(values) < 2 {
		return mean, 0
	}
	se := StdDev(values) / math.Sqrt(float64(len(values)))
	return mean, z * se
}

// Z95 is the standard normal critical value for a 95% two-sided interval.
const Z95 = 1.959964

// Summarize computes the full statistic set the engine needs from one series.
func Summarize(values []float64, window time.Duration, method PercentileMethod) Summary {
	s := Summary{Samples: len(values), WindowDuration: window}
	if len(values) == 0 {
		return s
	}
	qs := Percentiles(values, []float64{0.5, 0.9, 0.95, 0.99}, method)
	s.Mean = Mean(values)
	s.StdDev = StdDev(values)
	s.Min = Min(values)
	s.P50, s.P90, s.P95, s.P99 = qs[0], qs[1], qs[2], qs[3]
	s.Max = Max(values)
	if s.Mean > 0 {
		s.Burstiness = s.Max / s.Mean
	}
	return s
}

// Summary mirrors model.ObservedStats but lives here to keep the stats package
// free of a dependency on the domain model. The recommender converts between
// them.
type Summary struct {
	Samples        int
	Mean           float64
	StdDev         float64
	Min            float64
	P50            float64
	P90            float64
	P95            float64
	P99            float64
	Max            float64
	Burstiness     float64
	WindowDuration time.Duration
}
