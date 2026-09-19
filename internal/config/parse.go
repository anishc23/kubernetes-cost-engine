package config

import (
	"fmt"

	"github.com/anishc23/k8s-cost-optimizer/pkg/quantity"
)

func parseCPUFloor(s string) (float64, error) {
	v, err := quantity.ParseCPU(s)
	if err != nil {
		return 0, fmt.Errorf("policy.cpu_floor: %w", err)
	}
	if v < 0 {
		return 0, fmt.Errorf("policy.cpu_floor %q is negative", s)
	}
	return float64(v), nil
}

func parseMemoryFloor(s string) (float64, error) {
	v, err := quantity.ParseMemory(s)
	if err != nil {
		return 0, fmt.Errorf("policy.memory_floor: %w", err)
	}
	if v < 0 {
		return 0, fmt.Errorf("policy.memory_floor %q is negative", s)
	}
	return float64(v), nil
}
