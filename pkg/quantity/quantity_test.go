package quantity

import (
	"testing"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

func TestParseCPU(t *testing.T) {
	cases := []struct {
		in   string
		want model.Millicores
	}{
		{"100m", 100},
		{"1", 1000},
		{"2.5", 2500},
		{"1500m", 1500},
		{"0", 0},
		{"250m", 250},
	}
	for _, c := range cases {
		got, err := ParseCPU(c.in)
		if err != nil {
			t.Fatalf("ParseCPU(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseCPU(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := ParseCPU("not-a-quantity"); err == nil {
		t.Error("expected error for invalid CPU quantity")
	}
}

func TestParseMemory(t *testing.T) {
	cases := []struct {
		in   string
		want model.Bytes
	}{
		{"1Gi", 1073741824},
		{"512Mi", 536870912},
		{"1024Ki", 1048576},
		{"1G", 1000000000}, // decimal SI, deliberately different from 1Gi
		{"0", 0},
	}
	for _, c := range cases {
		got, err := ParseMemory(c.in)
		if err != nil {
			t.Fatalf("ParseMemory(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseMemory(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := ParseMemory("12XiB"); err == nil {
		t.Error("expected error for invalid memory quantity")
	}
}

// Rounding must never reduce a value: the safety margin is applied before
// rounding, so rounding down would silently erode it.
func TestRoundingNeverDecreases(t *testing.T) {
	for _, v := range []model.Millicores{1, 9, 10, 11, 99, 100, 101, 999, 1000, 1001, 12345.6} {
		got := RoundCPUUp(v)
		if got < v {
			t.Errorf("RoundCPUUp(%v) = %v, which is less than input", v, got)
		}
		if float64(got) >= float64(v)+CPUGranularityMilli {
			t.Errorf("RoundCPUUp(%v) = %v, rounded up by more than one step", v, got)
		}
		if int64(got)%CPUGranularityMilli != 0 {
			t.Errorf("RoundCPUUp(%v) = %v, not on a %dm boundary", v, got, CPUGranularityMilli)
		}
	}
	for _, v := range []model.Bytes{1, 1024, 1048575, 1048576, 1048577, 1610612736.7} {
		got := RoundMemoryUp(v)
		if got < v {
			t.Errorf("RoundMemoryUp(%v) = %v, which is less than input", v, got)
		}
		if int64(got)%MemoryGranularityBytes != 0 {
			t.Errorf("RoundMemoryUp(%v) = %v, not on a MiB boundary", v, got)
		}
	}
}

func TestRoundCPUUpExactValues(t *testing.T) {
	cases := []struct{ in, want model.Millicores }{
		{0, 0},
		{-5, 0},
		{1, 10},
		{10, 10},
		{11, 20},
		{355, 360},
		{1000, 1000},
	}
	for _, c := range cases {
		if got := RoundCPUUp(c.in); got != c.want {
			t.Errorf("RoundCPUUp(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRoundMemoryUpExactValues(t *testing.T) {
	mi := model.Bytes(model.BytesPerMi)
	cases := []struct{ in, want model.Bytes }{
		{0, 0},
		{-1, 0},
		{1, mi},
		{mi, mi},
		{mi + 1, 2 * mi},
		{734003200, 700 * mi}, // 700Mi exactly
	}
	for _, c := range cases {
		if got := RoundMemoryUp(c.in); got != c.want {
			t.Errorf("RoundMemoryUp(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCPUString(t *testing.T) {
	cases := []struct {
		in   model.Millicores
		want string
	}{
		{0, "0"},
		{-10, "0"},
		{100, "100m"},
		{1000, "1"},
		{2000, "2"},
		{1500, "1500m"},
		{360, "360m"},
	}
	for _, c := range cases {
		if got := CPUString(c.in); got != c.want {
			t.Errorf("CPUString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMemoryString(t *testing.T) {
	cases := []struct {
		in   model.Bytes
		want string
	}{
		{0, "0"},
		{1073741824, "1Gi"},
		{536870912, "512Mi"},
		{1610612736, "1536Mi"}, // 1.5Gi renders as MiB for comparability
		{1048576, "1Mi"},
		{2048, "2Ki"},
		{1500, "1.5Ki"}, // not evenly divisible: rendered readably, still a valid quantity
	}
	for _, c := range cases {
		if got := MemoryString(c.in); got != c.want {
			t.Errorf("MemoryString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Round-tripping through Kubernetes quantities must be lossless for values the
// engine actually emits, since recommendations are rendered as quantities in
// the API and may be read back by an applier.
func TestQuantityRoundTrip(t *testing.T) {
	for _, m := range []model.Millicores{10, 100, 355, 1000, 2500} {
		q := CPUQuantity(m)
		if got := CPUFromQuantity(q); got != m {
			t.Errorf("CPU round trip %v -> %v", m, got)
		}
	}
	for _, b := range []model.Bytes{1048576, 536870912, 1073741824, 1610612736} {
		q := MemoryQuantity(b)
		if got := MemoryFromQuantity(q); got != b {
			t.Errorf("memory round trip %v -> %v", b, got)
		}
	}
}

func TestUnitHelpers(t *testing.T) {
	if got := model.Millicores(1500).Cores(); got != 1.5 {
		t.Errorf("Cores() = %v want 1.5", got)
	}
	if got := model.Bytes(1073741824).Gibibytes(); got != 1 {
		t.Errorf("Gibibytes() = %v want 1", got)
	}
	if got := model.Bytes(536870912).Mebibytes(); got != 512 {
		t.Errorf("Mebibytes() = %v want 512", got)
	}
}

// Observed statistics are never round numbers of bytes. Rendering them as raw
// byte counts beside a recommendation in MiB makes the two incomparable, which
// defeats the purpose of showing the evidence.
func TestMemoryStringRendersNonRoundValuesReadably(t *testing.T) {
	cases := []struct {
		in   model.Bytes
		want string
	}{
		{229332309, "218.7Mi"}, // an observed mean working set
		{434124390, "414.0Mi"},
		{1500, "1.5Ki"},
		{500, "500"}, // below a KiB: raw is the only sensible form
	}
	for _, c := range cases {
		if got := MemoryString(c.in); got != c.want {
			t.Errorf("MemoryString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	// Exact values must keep their exact rendering.
	for _, c := range []struct {
		in   model.Bytes
		want string
	}{
		{1073741824, "1Gi"},
		{536870912, "512Mi"},
		{1610612736, "1536Mi"},
	} {
		if got := MemoryString(c.in); got != c.want {
			t.Errorf("MemoryString(%v) = %q, want %q (exact values must not gain a decimal)", c.in, got, c.want)
		}
	}
}

// Two statistics of the same series must render in the same unit, or they cannot
// be compared at a glance — which is the point of showing them together.
func TestMemoryStringUsesMiBConsistentlyAboveOneMiB(t *testing.T) {
	// 334856Ki is Ki-divisible but well above a MiB; it must not render in Ki
	// beside a mean rendered in MiB.
	if got := MemoryString(334856 * 1024); got != "327.0Mi" {
		t.Errorf("MemoryString(334856Ki) = %q, want MiB rendering", got)
	}
	// Sub-MiB values still use Ki, where it is the readable unit.
	if got := MemoryString(2048); got != "2Ki" {
		t.Errorf("MemoryString(2048) = %q, want 2Ki", got)
	}
}

// Whatever form it takes, the output must remain a valid Kubernetes quantity so
// that nothing downstream has to special-case it.
func TestMemoryStringAlwaysParsesAsAQuantity(t *testing.T) {
	for _, v := range []model.Bytes{
		1, 500, 1500, 1048576, 229332309, 434124390, 1073741824, 1610612736,
	} {
		s := MemoryString(v)
		if _, err := ParseMemory(s); err != nil {
			t.Errorf("MemoryString(%v) = %q, which is not a parseable quantity: %v", v, s, err)
		}
	}
}
