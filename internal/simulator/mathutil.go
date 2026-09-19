package simulator

import (
	"math"
	"sort"
)

// These helpers duplicate a little of internal/stats deliberately. The
// simulator computes ground truth, and ground truth must not depend on the same
// estimator the engine under test uses: if a bug in the percentile estimator
// affected both the recommendation and the reference it is scored against, the
// error would cancel and the experiment would report success. The two
// implementations are cross-checked in simulator tests.
func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func maxOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	m := v[0]
	for _, x := range v[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

// pctl is a nearest-rank percentile over the demand trace. Nearest-rank is used
// for ground truth because a required-resource envelope should be a level the
// workload actually reached, not an interpolation between two levels it reached.
func pctl(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := make([]float64, len(v))
	copy(s, v)
	sort.Float64s(s)
	rank := int(math.Ceil(p * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}
