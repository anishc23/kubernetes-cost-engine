package stats

import (
	"math/rand"
	"testing"
	"time"
)

// These benchmarks exist to answer one operational question: at what cluster size
// does the analysis become compute-bound rather than query-bound?
//
// The answer shapes the scaling guidance in docs/architecture.md, which claims
// that Prometheus query volume is the binding constraint rather than optimizer
// CPU. That claim should rest on a measurement, not an assumption.
//
// Sizes are chosen to match real observation windows at a 1-minute step:
//   1440   =  1 day
//   10080  =  7 days
//   43200  = 30 days

func series(n int) []float64 {
	rng := rand.New(rand.NewSource(1))
	out := make([]float64, n)
	for i := range out {
		out[i] = 100 + rng.NormFloat64()*20
	}
	return out
}

func BenchmarkPercentile(b *testing.B) {
	for _, n := range []int{1440, 10080, 43200} {
		vals := series(n)
		b.Run(sizeName(n)+"/linear", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Percentile(vals, 0.95, LinearInterpolation)
			}
		})
		b.Run(sizeName(n)+"/nearest", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Percentile(vals, 0.95, NearestRank)
			}
		})
	}
}

// Percentiles computes several quantiles from one sort. The engine always needs
// p50/p90/p95/p99 together, so this is the path that actually runs; comparing it
// against four separate calls shows whether the single-sort optimisation earns
// its place.
func BenchmarkPercentilesVersusIndividual(b *testing.B) {
	vals := series(10080)
	ps := []float64{0.5, 0.9, 0.95, 0.99}

	b.Run("batched", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = Percentiles(vals, ps, LinearInterpolation)
		}
	})
	b.Run("individual", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, p := range ps {
				_ = Percentile(vals, p, LinearInterpolation)
			}
		}
	})
}

// Summarize is the per-container, per-resource cost of an analysis cycle.
// Multiply by 2 resources x containers to estimate a cycle's compute.
func BenchmarkSummarize(b *testing.B) {
	for _, n := range []int{1440, 10080, 43200} {
		vals := series(n)
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Summarize(vals, 7*24*time.Hour, LinearInterpolation)
			}
		})
	}
}

func BenchmarkMeanKahan(b *testing.B) {
	vals := series(10080)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Mean(vals)
	}
}

func sizeName(n int) string {
	switch n {
	case 1440:
		return "1day"
	case 10080:
		return "7day"
	case 43200:
		return "30day"
	default:
		return "n"
	}
}
