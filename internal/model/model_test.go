package model

import (
	"testing"
	"time"
)

func mkSeries(n int, step time.Duration, value func(i int) float64) Series {
	end := time.Date(2025, 1, 6, 12, 0, 0, 0, time.UTC)
	s := make([]Sample, n)
	for i := range s {
		s[i] = Sample{
			Timestamp: end.Add(-time.Duration(n-1-i) * step),
			Value:     value(i),
		}
	}
	return Series{Resource: ResourceCPU, Samples: s, Step: step}
}

// Window is how the observation-window study (RQ6) is implemented: changing one
// policy field rather than re-collecting data. It must select the *most recent*
// portion, or every window result would describe the wrong period.
func TestWindowSelectsTheMostRecentPortion(t *testing.T) {
	// 120 samples at 1 minute: value == index, so recency is identifiable.
	s := mkSeries(120, time.Minute, func(i int) float64 { return float64(i) })

	w := s.Window(30 * time.Minute)
	if w.Len() == 0 || w.Len() >= s.Len() {
		t.Fatalf("30m window of a 2h series selected %d of %d samples", w.Len(), s.Len())
	}
	// It must end where the full series ends.
	if got, want := w.Samples[w.Len()-1], s.Samples[s.Len()-1]; got != want {
		t.Errorf("window ends at %v, full series ends at %v", got, want)
	}
	// And it must contain only recent values.
	if first := w.Samples[0].Value; first < 80 {
		t.Errorf("window starts at value %v, which is not from the recent portion", first)
	}
	if w.Duration() > 30*time.Minute+time.Minute {
		t.Errorf("window duration %v exceeds the request", w.Duration())
	}
}

func TestWindowEdgeCases(t *testing.T) {
	s := mkSeries(10, time.Minute, func(i int) float64 { return 1 })

	// Asking for more than exists returns everything rather than erroring.
	if got := s.Window(365 * 24 * time.Hour); got.Len() != s.Len() {
		t.Errorf("oversized window returned %d of %d samples", got.Len(), s.Len())
	}
	// An empty series windows to an empty series.
	empty := Series{Resource: ResourceCPU, Step: time.Minute}
	if got := empty.Window(time.Hour); got.Len() != 0 {
		t.Errorf("windowing an empty series produced %d samples", got.Len())
	}
	// Window must not mutate the receiver.
	before := s.Len()
	_ = s.Window(2 * time.Minute)
	if s.Len() != before {
		t.Error("Window mutated the original series")
	}
}

func TestSeriesDurationAndValues(t *testing.T) {
	s := mkSeries(5, time.Minute, func(i int) float64 { return float64(i) * 10 })
	if got := s.Duration(); got != 4*time.Minute {
		t.Errorf("Duration() = %v, want 4m for 5 samples at 1m", got)
	}
	vals := s.Values()
	if len(vals) != 5 || vals[0] != 0 || vals[4] != 40 {
		t.Errorf("Values() = %v", vals)
	}
	// Fewer than two samples spans no time.
	if got := (Series{Samples: []Sample{{}}}).Duration(); got != 0 {
		t.Errorf("single-sample Duration() = %v, want 0", got)
	}
	if got := (Series{}).Duration(); got != 0 {
		t.Errorf("empty Duration() = %v, want 0", got)
	}
}

// A missing evidence entry is not the same as zero restarts: absent evidence must
// make the engine more conservative, and the caller needs to tell the difference.
func TestEvidenceForDistinguishesAbsentFromZero(t *testing.T) {
	w := Workload{
		Namespace: "app", Name: "api",
		Evidence: map[string]RestartEvidence{
			"app": {Restarts: 0, OOMKills: 0},
		},
	}
	if ev, ok := w.EvidenceFor("app"); !ok || ev.OOMKills != 0 {
		t.Errorf("present-but-zero evidence: got %+v, ok=%v", ev, ok)
	}
	if _, ok := w.EvidenceFor("sidecar"); ok {
		t.Error("a container with no evidence entry must report ok=false")
	}
	// A nil map must not panic.
	if _, ok := (Workload{}).EvidenceFor("app"); ok {
		t.Error("a workload with no evidence map should report ok=false")
	}
}

func TestKeys(t *testing.T) {
	w := Workload{Namespace: "prod", Name: "api"}
	if got := w.Key(); got != "prod/api" {
		t.Errorf("Workload.Key() = %q", got)
	}
	r := WorkloadRecommendation{Namespace: "prod", Name: "api"}
	if got := r.Key(); got != "prod/api" {
		t.Errorf("WorkloadRecommendation.Key() = %q", got)
	}
	// The two must agree, since results are joined on this key.
	if w.Key() != r.Key() {
		t.Error("workload and recommendation keys must match")
	}
}

func TestUnitConversions(t *testing.T) {
	if got := Millicores(1500).Cores(); got != 1.5 {
		t.Errorf("Cores() = %v, want 1.5", got)
	}
	if got := Bytes(1073741824).Gibibytes(); got != 1 {
		t.Errorf("Gibibytes() = %v, want 1", got)
	}
	if got := Bytes(536870912).Mebibytes(); got != 512 {
		t.Errorf("Mebibytes() = %v, want 512", got)
	}
	if got := Millicores(250).String(); got != "250m" {
		t.Errorf("Millicores.String() = %q", got)
	}
	if got := Bytes(512 * 1024 * 1024).String(); got != "512Mi" {
		t.Errorf("Bytes.String() = %q", got)
	}
}

// An unset limit must be representable as distinct from zero: an unset CPU limit
// means the container may burst, while a zero one would mean it may not, and the
// risk model depends on the difference.
func TestResourceLimitsDistinguishUnsetFromZero(t *testing.T) {
	zero := Millicores(0)
	withLimit := ResourceRequests{CPURequest: 100, CPULimit: &zero}
	withoutLimit := ResourceRequests{CPURequest: 100}

	if withoutLimit.CPULimit != nil {
		t.Error("an unset CPU limit must be nil")
	}
	if withLimit.CPULimit == nil {
		t.Fatal("an explicitly zero limit must be representable")
	}
	if *withLimit.CPULimit != 0 {
		t.Errorf("explicit zero limit = %v", *withLimit.CPULimit)
	}
}
