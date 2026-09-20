// Package quantity converts between the engine's internal float units and
// Kubernetes resource.Quantity values.
//
// This conversion is isolated in one package because it is where a subtle class
// of bug lives: a recommendation that is statistically correct but rounds down
// to a value below observed usage has created a reliability risk through
// arithmetic alone. Every rounding operation here is therefore direction-aware,
// and rounding never decreases a recommendation below its computed target.
package quantity

import (
	"fmt"
	"math"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

// CPUFromQuantity converts a Kubernetes CPU quantity to millicores.
func CPUFromQuantity(q resource.Quantity) model.Millicores {
	return model.Millicores(q.MilliValue())
}

// MemoryFromQuantity converts a Kubernetes memory quantity to bytes.
func MemoryFromQuantity(q resource.Quantity) model.Bytes {
	return model.Bytes(q.Value())
}

// CPUGranularityMilli is the granularity CPU recommendations are rounded to.
//
// 10m is chosen rather than 1m because the CFS quota period is 100ms by
// default, so 10m corresponds to 1ms of quota per period: finer values are
// below the resolution at which the kernel actually enforces the request, and
// emitting them would imply precision the system does not have.
const CPUGranularityMilli = 10

// MemoryGranularityBytes is the granularity memory recommendations are rounded
// to. Memory is rounded to whole mebibytes because that is the smallest unit
// operators write in practice, and sub-MiB precision is noise relative to page
// cache and allocator behaviour.
const MemoryGranularityBytes = 1024 * 1024

// RoundCPUUp rounds millicores up to the next CPU granularity step.
//
// Rounding is always upward: the target has already had its safety factor
// applied, and rounding down would erode that margin.
func RoundCPUUp(m model.Millicores) model.Millicores {
	if m <= 0 {
		return 0
	}
	steps := math.Ceil(float64(m) / CPUGranularityMilli)
	return model.Millicores(steps * CPUGranularityMilli)
}

// RoundMemoryUp rounds bytes up to the next whole mebibyte.
func RoundMemoryUp(b model.Bytes) model.Bytes {
	if b <= 0 {
		return 0
	}
	steps := math.Ceil(float64(b) / MemoryGranularityBytes)
	return model.Bytes(steps * MemoryGranularityBytes)
}

// CPUString renders millicores in the form operators write: whole cores above
// 1000m when the value divides evenly, millicores otherwise.
func CPUString(m model.Millicores) string {
	if m <= 0 {
		return "0"
	}
	mv := int64(math.Round(float64(m)))
	if mv%1000 == 0 {
		return fmt.Sprintf("%d", mv/1000)
	}
	return fmt.Sprintf("%dm", mv)
}

// MemoryString renders bytes using the largest binary suffix that divides the
// value evenly, so that 1536Mi is not reported as 1.5Gi (which Kubernetes
// accepts but operators find harder to compare).
//
// Values that divide no suffix evenly — observed statistics such as a mean
// working set, which are never round — are rendered in MiB with one decimal
// place rather than as a raw byte count. A recommendation of "1536Mi" beside an
// observed mean of "229332309" is not a comparison a reader can make. The
// decimal form remains a valid Kubernetes quantity, so nothing downstream has to
// special-case it.
func MemoryString(b model.Bytes) string {
	if b <= 0 {
		return "0"
	}
	v := int64(math.Round(float64(b)))
	switch {
	case v%model.BytesPerGi == 0:
		return fmt.Sprintf("%dGi", v/model.BytesPerGi)
	case v%model.BytesPerMi == 0:
		return fmt.Sprintf("%dMi", v/model.BytesPerMi)
	case v >= model.BytesPerMi:
		// Anything at or above a mebibyte renders in MiB, even when it happens to
		// divide evenly into kibibytes. Checking Ki-divisibility first would print
		// an observed maximum as "334856Ki" next to a mean of "294.2Mi", and two
		// statistics of the same series in different units cannot be compared at a
		// glance — which is the entire purpose of showing them together.
		return fmt.Sprintf("%.1fMi", float64(v)/float64(model.BytesPerMi))
	case v%model.BytesPerKi == 0:
		return fmt.Sprintf("%dKi", v/model.BytesPerKi)
	case v >= model.BytesPerKi:
		return fmt.Sprintf("%.1fKi", float64(v)/float64(model.BytesPerKi))
	default:
		return fmt.Sprintf("%d", v)
	}
}

// ParseCPU parses a Kubernetes CPU string into millicores.
func ParseCPU(s string) (model.Millicores, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("parse cpu %q: %w", s, err)
	}
	return CPUFromQuantity(q), nil
}

// ParseMemory parses a Kubernetes memory string into bytes.
func ParseMemory(s string) (model.Bytes, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("parse memory %q: %w", s, err)
	}
	return MemoryFromQuantity(q), nil
}

// CPUQuantity converts millicores back to a Kubernetes quantity.
func CPUQuantity(m model.Millicores) resource.Quantity {
	return *resource.NewMilliQuantity(int64(math.Round(float64(m))), resource.DecimalSI)
}

// MemoryQuantity converts bytes back to a Kubernetes quantity.
func MemoryQuantity(b model.Bytes) resource.Quantity {
	return *resource.NewQuantity(int64(math.Round(float64(b))), resource.BinarySI)
}
