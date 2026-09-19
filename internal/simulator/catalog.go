package simulator

import (
	"fmt"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

// Catalog is the canonical set of workload specifications used in the study.
//
// The parameters are not arbitrary. Each class is calibrated so that the
// experiment can distinguish between hypotheses rather than merely produce
// numbers:
//
//   - Declared requests are over-provisioned by a factor that varies by class
//     (2x to 12x), because RQ1 asks how much waste exists and a single
//     over-provisioning factor would make that question trivial.
//   - bursty-cpu and spiky-cpu have similar max/mean ratios but very different
//     burst durations, so that a percentile policy's failure can be attributed to
//     peak *duration* rather than peak *height*.
//   - periodic-cpu reaches the same peak as bursty-cpu but spends far more
//     samples near it, which should make identical percentile policies behave
//     differently on the two classes. If they do not, the workload-class
//     hypothesis (RQ4) is not supported and the study should say so.
//
// Every value below is a ground-truth parameter, so the tables in
// research/experimental_setup.md are generated from this function rather than
// transcribed from it.
func Catalog(duration, step time.Duration, seed int64) []Spec {
	mi := float64(model.BytesPerMi)
	specs := []Spec{
		{
			Class: ClassStableCPU, Name: "stable-cpu",
			BaselineCPU: 200, CPUNoiseFrac: 0.08,
			BaselineMemory: 256 * mi, MemoryNoiseFrac: 0.03,
			// 5x CPU over-provisioning: a common outcome of copying a request
			// from another service's manifest.
			DeclaredCPU: 1000, DeclaredMemory: 1024 * mi,
		},
		{
			Class: ClassBurstyCPU, Name: "bursty-cpu",
			BaselineCPU: 80, BurstCPU: 1400, CPUNoiseFrac: 0.12,
			// Duty cycle 30s/15min = 3.3%. Expressed as a duty cycle rather than
			// a sample count because the fraction of *samples* in a burst depends
			// on the sampling step, while the duty cycle is a property of the
			// workload. At 3.3%, bursts sit above p95 and partially within p99:
			// this class separates "p95 misses the peak" from "p99 misses it too".
			BurstDuration: 30 * time.Second, BurstInterval: 15 * time.Minute, BurstJitter: 0.8,
			BaselineMemory: 320 * mi, MemoryNoiseFrac: 0.04,
			DeclaredCPU: 2000, DeclaredMemory: 1024 * mi,
		},
		{
			Class: ClassPeriodicCPU, Name: "periodic-cpu",
			BaselineCPU: 150, BurstCPU: 1400, CPUNoiseFrac: 0.10,
			CycleLength:    24 * time.Hour,
			BaselineMemory: 384 * mi, MemoryNoiseFrac: 0.05,
			// Same peak as bursty-cpu and the same declared request, so the two
			// classes are directly comparable.
			DeclaredCPU: 2000, DeclaredMemory: 1024 * mi,
		},
		{
			Class: ClassSpikyCPU, Name: "spiky-cpu",
			BaselineCPU: 60, BurstCPU: 2400, CPUNoiseFrac: 0.10,
			// Duty cycle 30s/4h = 0.2%, an order of magnitude rarer than
			// bursty-cpu. This places the spikes below p99 by construction, so
			// this class is where every percentile policy short of the maximum
			// must under-provision. It is included to establish that the
			// percentile ranking is not universal, which is the claim RQ2 tests.
			BurstDuration: 30 * time.Second, BurstInterval: 4 * time.Hour, BurstJitter: 0.9,
			BaselineMemory: 256 * mi, MemoryNoiseFrac: 0.04,
			DeclaredCPU: 3000, DeclaredMemory: 1024 * mi,
		},
		{
			Class: ClassStableMemory, Name: "stable-memory",
			BaselineCPU: 120, CPUNoiseFrac: 0.06,
			BaselineMemory: 700 * mi, MemoryNoiseFrac: 0.02,
			DeclaredCPU: 500, DeclaredMemory: 2048 * mi,
		},
		{
			Class: ClassGrowingMemory, Name: "growing-memory",
			BaselineCPU: 140, CPUNoiseFrac: 0.07,
			BaselineMemory: 500 * mi, MemoryNoiseFrac: 0.02,
			// 2.2x growth across the window: fast enough that a recommendation
			// fitted to the first hours is wrong by the end, which is the point.
			GrowthFactor: 2.2,
			DeclaredCPU:  500, DeclaredMemory: 2048 * mi,
		},
		{
			Class: ClassBurstyMemory, Name: "bursty-memory",
			BaselineCPU: 130, CPUNoiseFrac: 0.08,
			BaselineMemory: 400 * mi, PeakMemory: 1500 * mi, MemoryNoiseFrac: 0.03,
			// Duty cycle 2min/2h = 1.7%: transient allocations rare enough to sit
			// above p95. Memory is the resource where missing such a peak is
			// fatal rather than merely slow, which is what makes this class the
			// sharpest test of the CPU/memory asymmetry.
			BurstDuration: 2 * time.Minute, BurstInterval: 2 * time.Hour, BurstJitter: 0.7,
			DeclaredCPU: 500, DeclaredMemory: 3072 * mi,
		},
		{
			Class: ClassSawtoothMemory, Name: "sawtooth-memory",
			BaselineCPU: 150, BurstCPU: 600, CPUNoiseFrac: 0.08,
			BaselineMemory: 300 * mi, PeakMemory: 1400 * mi, MemoryNoiseFrac: 0.02,
			CycleLength: 20 * time.Minute,
			DeclaredCPU: 800, DeclaredMemory: 3072 * mi,
		},
		{
			Class: ClassMixed, Name: "mixed",
			BaselineCPU: 200, BurstCPU: 1200, CPUNoiseFrac: 0.10,
			BaselineMemory: 500 * mi, PeakMemory: 1200 * mi, MemoryNoiseFrac: 0.04,
			CycleLength:   12 * time.Hour,
			BurstDuration: time.Minute, BurstInterval: 20 * time.Minute, BurstJitter: 0.8,
			DeclaredCPU: 2000, DeclaredMemory: 2048 * mi,
		},
		{
			Class: ClassIdle, Name: "idle",
			BaselineCPU: 5, CPUNoiseFrac: 0.30,
			BaselineMemory: 40 * mi, MemoryNoiseFrac: 0.05,
			// 100x CPU over-provisioning. Extreme by design: idle workloads with
			// inherited requests are the largest single source of waste in real
			// clusters, and they are also where percentile policies produce
			// absurdly small targets that the floor gate must catch.
			DeclaredCPU: 500, DeclaredMemory: 512 * mi,
		},
	}
	for i := range specs {
		specs[i].Duration = duration
		specs[i].Step = step
		// Seeds are derived per class so that adding a class does not perturb
		// the traces of existing ones.
		specs[i].Seed = seed + int64(i)*7919 // a prime stride, to avoid seed aliasing
	}
	return specs
}

// SpecByClass returns the catalog entry for a class.
func SpecByClass(c Class, duration, step time.Duration, seed int64) (Spec, error) {
	for _, s := range Catalog(duration, step, seed) {
		if s.Class == c {
			return s, nil
		}
	}
	return Spec{}, fmt.Errorf("no catalog entry for workload class %q", c)
}

// GenerateCatalog generates every catalog trace.
func GenerateCatalog(duration, step time.Duration, seed int64) ([]Trace, error) {
	specs := Catalog(duration, step, seed)
	out := make([]Trace, 0, len(specs))
	for _, s := range specs {
		tr, err := Generate(s)
		if err != nil {
			return nil, fmt.Errorf("generate %s: %w", s.Name, err)
		}
		out = append(out, tr)
	}
	return out, nil
}
