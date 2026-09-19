package kube

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/pkg/quantity"
)

// types_StrategicMergePatchType is aliased so discovery.go reads without an
// import of k8s.io/apimachinery/pkg/types in two places.
const types_StrategicMergePatchType = types.StrategicMergePatchType

// buildResourcePatch produces a strategic-merge patch setting resource requests
// on the containers with actionable recommendations.
//
// Only DECREASE and INCREASE are applied. NO_CHANGE needs no patch;
// INSUFFICIENT_DATA and BLOCKED must never be applied — those are precisely the
// cases where the engine declined to make a claim, and turning them into a
// cluster write would defeat the safety machinery entirely.
//
// Limits are deliberately left untouched. A memory limit change alters when the
// kernel kills the process, and nothing in a usage series justifies making that
// change without a human deciding it.
func buildResourcePatch(rec model.WorkloadRecommendation) ([]byte, error) {
	type resources struct {
		Requests map[string]string `json:"requests,omitempty"`
	}
	type container struct {
		Name      string    `json:"name"`
		Resources resources `json:"resources"`
	}

	var containers []container
	for _, c := range rec.Containers {
		reqs := map[string]string{}
		if actionable(c.CPU.Decision) {
			reqs["cpu"] = quantity.CPUString(model.Millicores(c.CPU.Target))
		}
		if actionable(c.Memory.Decision) {
			reqs["memory"] = quantity.MemoryString(model.Bytes(c.Memory.Target))
		}
		if len(reqs) == 0 {
			continue
		}
		containers = append(containers, container{Name: c.Container, Resources: resources{Requests: reqs}})
	}
	if len(containers) == 0 {
		return nil, nil
	}

	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": containers,
				},
			},
		},
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("marshal resource patch: %w", err)
	}
	return b, nil
}

func actionable(d model.Decision) bool {
	return d == model.DecisionDecrease || d == model.DecisionIncrease
}

// resourceListFor is a helper used by tests to construct container resources.
func resourceListFor(cpu, memory string) corev1.ResourceRequirements {
	rr := corev1.ResourceRequirements{Requests: corev1.ResourceList{}}
	if cpu != "" {
		rr.Requests[corev1.ResourceCPU] = mustQuantity(cpu)
	}
	if memory != "" {
		rr.Requests[corev1.ResourceMemory] = mustQuantity(memory)
	}
	return rr
}
