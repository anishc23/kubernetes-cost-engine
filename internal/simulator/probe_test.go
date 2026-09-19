package simulator

import (
	"testing"
	"time"
)

// TestCatalogProfile is a diagnostic, not an assertion: it prints the demand
// profile of every class so that calibration decisions are visible and
// reviewable. Run with: go test ./internal/simulator -run Profile -v
func TestCatalogProfile(t *testing.T) {
	traces, err := GenerateCatalog(7*24*time.Hour, 30*time.Second, 42)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%-18s %6s %7s %7s %7s %7s %8s %8s %7s",
		"class", "n", "mean", "p95", "p99", "max", "p95/max", "p99/max", "peak%")
	for _, tr := range traces {
		g := tr.GroundTruth
		vals := tr.CPUDemand.Values()
		thresh := (g.CPUMeanMilli + g.CPUMaxMilli) / 2
		above := 0
		for _, v := range vals {
			if v > thresh {
				above++
			}
		}
		t.Logf("%-18s %6d %7.0f %7.0f %7.0f %7.0f %8.2f %8.2f %6.2f%%",
			tr.Spec.Name, len(vals), g.CPUMeanMilli, g.CPUP95Milli, g.CPUP99Milli, g.CPUMaxMilli,
			g.CPUP95Milli/g.CPUMaxMilli, g.CPUP99Milli/g.CPUMaxMilli,
			100*float64(above)/float64(len(vals)))
	}
	t.Logf("%-18s %10s %10s %10s %10s", "class", "mem mean", "mem p95", "mem p99", "mem max")
	for _, tr := range traces {
		g := tr.GroundTruth
		mi := 1024.0 * 1024.0
		t.Logf("%-18s %9.0fMi %9.0fMi %9.0fMi %9.0fMi", tr.Spec.Name,
			g.MemoryMeanBytes/mi, g.MemoryP95Bytes/mi, g.MemoryP99Bytes/mi, g.MemoryMaxBytes/mi)
	}
}
