package recommender

import (
	"testing"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

// Recommend is the whole per-workload analysis cost: summarise both resources,
// run both strategies, and evaluate the gate chain. Multiplying this by workload
// count gives the compute side of an analysis cycle, which is what the scaling
// argument in docs/architecture.md compares against Prometheus query time.
func BenchmarkRecommend(b *testing.B) {
	for _, n := range []int{1440, 10080} {
		p := DefaultPolicy()
		p.MinSamples = 10
		p.MinDuration = time.Minute
		p.ObservationWindow = 30 * 24 * time.Hour
		e, err := NewEngine(p)
		if err != nil {
			b.Fatal(err)
		}
		w := benchWorkload(n)
		name := "7day"
		if n == 1440 {
			name = "1day"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = e.Recommend(w)
			}
		})
	}
}

// A ten-container pod is unusual but not rare (sidecars, service mesh, log
// shippers), and cost scales with it.
func BenchmarkRecommendManyContainers(b *testing.B) {
	p := DefaultPolicy()
	p.MinSamples = 10
	p.MinDuration = time.Minute
	e, _ := NewEngine(p)

	w := benchWorkload(10080)
	base := w.Containers[0]
	for i := 1; i < 10; i++ {
		c := base
		c.Name = "sidecar"
		w.Containers = append(w.Containers, c)
		w.Evidence[c.Name] = model.RestartEvidence{}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = e.Recommend(w)
	}
}

func benchWorkload(n int) model.Workload {
	end := time.Now()
	step := time.Minute
	mk := func(kind model.ResourceKind, base float64) model.Series {
		s := make([]model.Sample, n)
		for i := range s {
			s[i] = model.Sample{
				Timestamp: end.Add(-time.Duration(n-1-i) * step),
				Value:     base * (0.9 + 0.2*float64(i%17)/17),
			}
		}
		return model.Series{Resource: kind, Samples: s, Step: step}
	}
	return model.Workload{
		Namespace: "bench", Name: "wl", Kind: model.KindDeployment, Replicas: 3,
		Containers: []model.Container{{
			Name: "app",
			Declared: model.ResourceRequests{
				CPURequest:    2000,
				MemoryRequest: model.Bytes(4 * model.BytesPerGi),
			},
			CPU:    mk(model.ResourceCPU, 200),
			Memory: mk(model.ResourceMemory, 500*float64(model.BytesPerMi)),
		}},
		Evidence: map[string]model.RestartEvidence{"app": {}},
	}
}
