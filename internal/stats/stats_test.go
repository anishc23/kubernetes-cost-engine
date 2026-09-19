package stats

import (
	"math"
	"testing"
	"time"
)

func approx(t *testing.T, got, want, tol float64, msg string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %v want %v (tol %v)", msg, got, want, tol)
	}
}

// Reference values cross-checked against numpy.percentile with the default
// (linear / R-7) interpolation, which is the estimator LinearInterpolation
// implements. See analysis/scripts/verify_percentiles.py for the generator.
func TestPercentileLinearMatchesNumpyReference(t *testing.T) {
	vals := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct{ p, want float64 }{
		{0.0, 1},
		{0.25, 3.25},
		{0.5, 5.5},
		{0.9, 9.1},
		{0.95, 9.55},
		{0.99, 9.91},
		{1.0, 10},
	}
	for _, c := range cases {
		approx(t, Percentile(vals, c.p, LinearInterpolation), c.want, 1e-9,
			"p="+fmtF(c.p))
	}
}

func fmtF(f float64) string { return time.Duration(f * 1e9).String() }

func TestPercentileNearestRankReturnsObservedValues(t *testing.T) {
	vals := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	// Nearest-rank must always return a value that actually appears in the
	// input: this is the property that makes it defensible for safety sizing.
	for _, p := range []float64{0.05, 0.25, 0.5, 0.9, 0.95, 0.99} {
		got := Percentile(vals, p, NearestRank)
		found := false
		for _, v := range vals {
			if v == got {
				found = true
			}
		}
		if !found {
			t.Errorf("nearest-rank p%.2f returned %v which is not an observed value", p, got)
		}
	}
	approx(t, Percentile(vals, 0.95, NearestRank), 10, 0, "p95 nearest-rank")
	approx(t, Percentile(vals, 0.5, NearestRank), 5, 0, "p50 nearest-rank")
}

// The two estimators must diverge on short windows: this is the mechanism
// behind the observation-window sensitivity studied in RQ6, so a regression
// here would silently invalidate that analysis.
func TestEstimatorsDivergeOnShortWindows(t *testing.T) {
	// 60 samples = 1 hour at 1-minute resolution, with a single large spike.
	vals := make([]float64, 60)
	for i := range vals {
		vals[i] = 100
	}
	vals[59] = 1000
	lin := Percentile(vals, 0.99, LinearInterpolation)
	near := Percentile(vals, 0.99, NearestRank)
	if near != 1000 {
		t.Errorf("nearest-rank p99 should capture the observed spike, got %v", near)
	}
	if lin >= 1000 {
		t.Errorf("linear p99 should interpolate below the spike, got %v", lin)
	}
	if lin <= 100 {
		t.Errorf("linear p99 should exceed the baseline, got %v", lin)
	}
}

func TestPercentileEmptyAndSingle(t *testing.T) {
	if got := Percentile(nil, 0.95, LinearInterpolation); got != 0 {
		t.Errorf("empty input: got %v want 0", got)
	}
	if got := Percentile([]float64{42}, 0.99, LinearInterpolation); got != 42 {
		t.Errorf("single sample: got %v want 42", got)
	}
	if got := Percentile([]float64{42}, 0.01, NearestRank); got != 42 {
		t.Errorf("single sample nearest-rank: got %v want 42", got)
	}
}

func TestPercentileDoesNotMutateInput(t *testing.T) {
	vals := []float64{5, 1, 4, 2, 3}
	orig := append([]float64(nil), vals...)
	_ = Percentile(vals, 0.9, LinearInterpolation)
	_ = Percentiles(vals, []float64{0.5, 0.95}, NearestRank)
	for i := range vals {
		if vals[i] != orig[i] {
			t.Fatalf("input mutated at %d: got %v want %v", i, vals[i], orig[i])
		}
	}
}

func TestPercentilesMatchesIndividualCalls(t *testing.T) {
	vals := []float64{3, 9, 1, 7, 5, 2, 8, 6, 4, 10, 11, 12}
	ps := []float64{0, 0.5, 0.9, 0.95, 0.99, 1}
	for _, m := range []PercentileMethod{LinearInterpolation, NearestRank} {
		batch := Percentiles(vals, ps, m)
		for i, p := range ps {
			approx(t, batch[i], Percentile(vals, p, m), 1e-12, "batch vs single")
		}
	}
}

func TestMeanKahanPrecisionOnLargeValues(t *testing.T) {
	// Memory series are in bytes: ~2 GiB values over 20k samples. Naive
	// float64 summation loses low-order bits; verify the mean is exact for a
	// constant series, which is the case naive summation gets visibly wrong.
	const v = 2 * 1024 * 1024 * 1024
	vals := make([]float64, 20000)
	for i := range vals {
		vals[i] = v
	}
	approx(t, Mean(vals), v, 1e-6, "mean of constant large series")
}

func TestStdDevIsSampleForm(t *testing.T) {
	vals := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	// Population sd is 2.0; sample (n-1) sd is 2.13809...
	approx(t, StdDev(vals), 2.1380899, 1e-6, "sample stddev")
	if got := StdDev([]float64{5}); got != 0 {
		t.Errorf("stddev of one sample: got %v want 0", got)
	}
}

func TestMinMaxEmpty(t *testing.T) {
	if Min(nil) != 0 || Max(nil) != 0 {
		t.Error("min/max of empty input should be 0")
	}
}

func TestCoefficientOfVariationZeroMean(t *testing.T) {
	if got := CoefficientOfVariation([]float64{0, 0, 0}); got != 0 {
		t.Errorf("cv with zero mean: got %v want 0", got)
	}
	approx(t, CoefficientOfVariation([]float64{10, 10, 10}), 0, 1e-12, "cv of constant")
}

func TestSummarizeBurstiness(t *testing.T) {
	vals := []float64{100, 100, 100, 100, 1000}
	s := Summarize(vals, time.Hour, LinearInterpolation)
	if s.Samples != 5 {
		t.Errorf("samples: got %d want 5", s.Samples)
	}
	approx(t, s.Max, 1000, 0, "max")
	approx(t, s.Mean, 280, 1e-9, "mean")
	approx(t, s.Burstiness, 1000.0/280.0, 1e-9, "burstiness")
	if s.WindowDuration != time.Hour {
		t.Errorf("window: got %v", s.WindowDuration)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	s := Summarize(nil, time.Hour, LinearInterpolation)
	if s.Samples != 0 || s.Mean != 0 || s.Burstiness != 0 {
		t.Errorf("empty summary should be zero-valued, got %+v", s)
	}
}

func TestMeanCI(t *testing.T) {
	vals := []float64{10, 12, 14, 16, 18}
	m, hw := MeanCI(vals, Z95)
	approx(t, m, 14, 1e-9, "mean")
	// sd = 3.1623, se = 1.4142, hw = 1.96*1.4142 = 2.7718
	approx(t, hw, 2.7718, 1e-3, "half width")
	if _, hw := MeanCI([]float64{5}, Z95); hw != 0 {
		t.Error("single-sample CI half-width should be 0")
	}
}

func TestPercentileMonotonicity(t *testing.T) {
	vals := []float64{1, 5, 2, 8, 3, 9, 4, 7, 6, 10, 100}
	for _, m := range []PercentileMethod{LinearInterpolation, NearestRank} {
		prev := math.Inf(-1)
		for p := 0.0; p <= 1.0; p += 0.01 {
			got := Percentile(vals, p, m)
			if got < prev-1e-9 {
				t.Fatalf("%s: percentile not monotonic at p=%.2f: %v < %v", m, p, got, prev)
			}
			prev = got
		}
	}
}
