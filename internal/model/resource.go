// Package model contains the core domain types shared by the production
// controller and the research experiment framework. Keeping a single set of
// types is deliberate: the recommendation engine must be exercised by exactly
// the same code paths in both modes, otherwise experimental results would not
// say anything about the deployed system.
package model

import (
	"fmt"
	"time"
)

// ResourceKind distinguishes the two resources the engine right-sizes. They are
// kept as distinct kinds rather than a generic "resource" because their failure
// semantics differ fundamentally (see docs/recommendation-algorithm.md):
//
//	CPU    is compressible: exceeding the request causes queueing/throttling.
//	Memory is incompressible: exceeding the limit causes an OOMKill.
type ResourceKind string

const (
	ResourceCPU    ResourceKind = "cpu"
	ResourceMemory ResourceKind = "memory"
)

// Canonical internal units.
//
// CPU is stored in millicores (1000m == 1 core) and memory in bytes. Both are
// float64 rather than integers so that statistical operations (percentiles,
// means, safety multiplications) do not accumulate rounding error; conversion
// back to Kubernetes quantities happens exactly once, at the edge, in
// pkg/quantity.
type (
	// Millicores is CPU expressed in milli-CPU units.
	Millicores float64
	// Bytes is memory expressed in bytes.
	Bytes float64
)

const (
	MilliPerCore       = 1000.0
	BytesPerKi   int64 = 1024
	BytesPerMi   int64 = 1024 * 1024
	BytesPerGi   int64 = 1024 * 1024 * 1024
)

func (m Millicores) Cores() float64 { return float64(m) / MilliPerCore }
func (b Bytes) Mebibytes() float64  { return float64(b) / float64(BytesPerMi) }
func (b Bytes) Gibibytes() float64  { return float64(b) / float64(BytesPerGi) }

func (m Millicores) String() string { return fmt.Sprintf("%.0fm", float64(m)) }
func (b Bytes) String() string      { return fmt.Sprintf("%.0fMi", b.Mebibytes()) }

// Sample is one observation of a scalar metric at a point in time.
//
// Value units depend on the series: millicores for CPU, bytes for memory.
type Sample struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// Series is an ordered set of samples for one (container, resource) pair.
//
// A Series is the single boundary between data acquisition and analysis. In
// production it is produced by internal/promapi from a Prometheus range query;
// in research mode it is produced by internal/simulator from a generative
// workload model with known ground truth. Nothing downstream can tell the
// difference, which is what makes the experiments relevant to the deployed
// engine.
type Series struct {
	Resource ResourceKind `json:"resource"`
	Samples  []Sample     `json:"samples"`
	// Step is the sampling interval the series was collected at. It is needed
	// to convert per-sample excess into resource-seconds and to reason about
	// what the sampling resolution can and cannot reveal (short bursts below
	// the step are invisible; see research/limitations.md).
	Step time.Duration `json:"step"`
}

func (s Series) Len() int { return len(s.Samples) }

// Values returns the raw sample values in timestamp order.
func (s Series) Values() []float64 {
	out := make([]float64, len(s.Samples))
	for i, sm := range s.Samples {
		out[i] = sm.Value
	}
	return out
}

// Duration is the wall-clock span covered by the series.
func (s Series) Duration() time.Duration {
	if len(s.Samples) < 2 {
		return 0
	}
	return s.Samples[len(s.Samples)-1].Timestamp.Sub(s.Samples[0].Timestamp)
}

// Window returns the sub-series covering the final d of the observation
// period. It is used to study the effect of observation-window length (RQ6)
// without re-collecting data.
func (s Series) Window(d time.Duration) Series {
	if len(s.Samples) == 0 {
		return s
	}
	cutoff := s.Samples[len(s.Samples)-1].Timestamp.Add(-d)
	idx := len(s.Samples)
	for i := len(s.Samples) - 1; i >= 0; i-- {
		if s.Samples[i].Timestamp.Before(cutoff) {
			break
		}
		idx = i
	}
	return Series{Resource: s.Resource, Samples: s.Samples[idx:], Step: s.Step}
}
