package model

import "time"

// WorkloadKind is the Kubernetes controller type that owns a set of pods.
type WorkloadKind string

const (
	KindDeployment  WorkloadKind = "Deployment"
	KindStatefulSet WorkloadKind = "StatefulSet"
	KindDaemonSet   WorkloadKind = "DaemonSet"
	KindCronJob     WorkloadKind = "CronJob"
	KindJob         WorkloadKind = "Job"
	// KindSynthetic marks a workload produced by the research-mode generator.
	// It exists so that simulated results can never be silently presented as
	// having come from a real cluster.
	KindSynthetic WorkloadKind = "Synthetic"
)

// ResourceRequests holds the declared request/limit pair for one container.
//
// A nil limit means "unset", which matters: an unset CPU limit means the
// container can burst above its request when the node has spare capacity,
// while an unset memory limit means the container is only OOMKilled under
// node-level pressure. The engine's risk model depends on this distinction, so
// "unset" must be representable rather than collapsed to zero.
type ResourceRequests struct {
	CPURequest    Millicores  `json:"cpu_request"`
	CPULimit      *Millicores `json:"cpu_limit,omitempty"`
	MemoryRequest Bytes       `json:"memory_request"`
	MemoryLimit   *Bytes      `json:"memory_limit,omitempty"`
}

// Container is one container within a workload, together with its declared
// resources and the usage history observed for it.
type Container struct {
	Name     string           `json:"name"`
	Declared ResourceRequests `json:"declared"`
	CPU      Series           `json:"-"`
	Memory   Series           `json:"-"`
}

// RestartEvidence summarises the reliability signals gathered for a container.
// It is the input to the safety gates that can veto a memory reduction.
type RestartEvidence struct {
	// Restarts is the total container restart count over the observation window.
	Restarts int `json:"restarts"`
	// OOMKills is the number of restarts whose termination reason was OOMKilled.
	// It is tracked separately from Restarts because only OOM evidence is
	// direct evidence of a memory sizing failure.
	OOMKills int `json:"oom_kills"`
	// LastOOM is the most recent OOMKill timestamp, if any.
	LastOOM *time.Time `json:"last_oom,omitempty"`
	// CPUThrottledRatio is throttled periods / elapsed periods from
	// container_cpu_cfs_throttled_periods_total, when available.
	CPUThrottledRatio float64 `json:"cpu_throttled_ratio"`
}

// Workload is the unit the engine reasons about: a controller-owned group of
// pods sharing one pod template, and therefore one set of resource requests.
//
// Recommendations are made per workload rather than per pod because the request
// lives in the pod template; recommending per pod would produce advice that
// cannot be applied.
type Workload struct {
	Namespace  string                     `json:"namespace"`
	Name       string                     `json:"name"`
	Kind       WorkloadKind               `json:"kind"`
	Replicas   int32                      `json:"replicas"`
	Labels     map[string]string          `json:"labels,omitempty"`
	Containers []Container                `json:"containers"`
	Evidence   map[string]RestartEvidence `json:"evidence,omitempty"`

	// WorkloadClass is the ground-truth generator class in research mode, and
	// empty in production unless a classifier has labelled it. It is recorded
	// so that results can be broken down by workload behaviour (RQ4).
	WorkloadClass string `json:"workload_class,omitempty"`
}

// Key is the stable identifier used in the API and in experiment records.
func (w Workload) Key() string { return w.Namespace + "/" + w.Name }

// EvidenceFor returns the reliability evidence for a container, and whether any
// was collected. A missing entry is not the same as zero restarts: absent
// evidence must make the engine more conservative, not less.
func (w Workload) EvidenceFor(container string) (RestartEvidence, bool) {
	e, ok := w.Evidence[container]
	return e, ok
}
