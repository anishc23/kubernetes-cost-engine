package simulator

import (
	"math"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

// Config controls how a candidate resource configuration is replayed against a
// demand trace. The defaults encode the pessimistic interpretation, so that a
// strategy is never credited for safety it would only enjoy on an idle node.
type Config struct {
	// CPURequestIsCeiling treats the CPU request as the amount the container can
	// actually obtain.
	//
	// This is a modelling decision that deserves scrutiny. In Kubernetes a CPU
	// request becomes a cpu.shares weight, not a cap: on a node with spare
	// capacity a container freely exceeds its request, and only a CPU *limit*
	// imposes a hard CFS quota. Treating the request as a ceiling therefore
	// models a fully contended node, where each container receives
	// approximately its share.
	//
	// It is the default because the alternative flatters every strategy equally
	// and uninformatively: if containers can always burst above their request,
	// then CPU under-provisioning has no measurable consequence in the model and
	// every strategy scores identically on CPU risk. Reporting under contention
	// is the assumption under which the CPU comparison carries information. The
	// consequence — that reported CPU violation rates are an upper bound on what
	// a well-provisioned cluster would experience — is stated in
	// research/threats_to_validity.md.
	CPURequestIsCeiling bool

	// MemoryLimitEqualsRequest replays memory recommendations as both request
	// and limit (the Guaranteed QoS pattern).
	//
	// When a container has no memory limit it is killed only under node-level
	// pressure, which depends on co-tenancy the simulator does not model. Setting
	// limit = request gives a well-defined, pessimistic bound: any excursion
	// above the recommendation is fatal. Results are therefore an upper bound on
	// OOM risk for Burstable pods and an accurate model for Guaranteed ones.
	MemoryLimitEqualsRequest bool

	// OOMCooldown is the minimum interval between two counted OOMKills.
	//
	// A single sustained excursion above the limit is one sizing failure, not one
	// per sample. Without a cooldown, a workload 10% above its limit for an hour
	// at 30s resolution would report 120 OOMKills, and the metric would measure
	// excursion length rather than failure count. The cooldown is set to a
	// plausible crash-loop backoff so that the count approximates the number of
	// distinct kill-restart episodes an operator would see.
	OOMCooldown time.Duration
}

// DefaultConfig returns the pessimistic replay configuration used for all
// reported results.
func DefaultConfig() Config {
	return Config{
		CPURequestIsCeiling:      true,
		MemoryLimitEqualsRequest: true,
		OOMCooldown:              2 * time.Minute,
	}
}

// Outcome is what actually happened when a configuration met the demand trace.
//
// Every field is derived from demand, not from the censored series the engine
// saw. That is the whole point: these are the numbers a recommendation is scored
// against, and the engine had no access to them.
type Outcome struct {
	CPURequestMilli    float64 `json:"cpu_request_milli"`
	MemoryRequestBytes float64 `json:"memory_request_bytes"`

	// --- CPU consequences ---

	// CPUViolationRate is the fraction of samples whose demand exceeded the
	// request. It counts how often the workload wanted more than it was
	// guaranteed, which is the frequency of degradation.
	CPUViolationRate float64 `json:"cpu_violation_rate"`
	// CPUThrottledCoreSeconds is the integral of unmet CPU demand. It measures
	// the magnitude of degradation, which violation rate alone cannot: being 1%
	// short for many samples and 400% short for one are very different failures.
	CPUThrottledCoreSeconds float64 `json:"cpu_throttled_core_seconds"`
	// CPUThrottleFraction is CPUThrottledCoreSeconds divided by total demanded
	// core-seconds: the share of requested work that was not served. This is the
	// scale-free CPU risk metric used in the trade-off curves.
	CPUThrottleFraction float64 `json:"cpu_throttle_fraction"`
	// CPUUtilization is mean served CPU over the request — the efficiency side
	// of the trade-off. It exceeds nothing by construction: it is bounded by 1.
	CPUUtilization float64 `json:"cpu_utilization"`
	// CPUMaxShortfallMilli is the largest single unmet demand, describing the
	// worst moment rather than the average one.
	CPUMaxShortfallMilli float64 `json:"cpu_max_shortfall_milli"`

	// --- Memory consequences ---

	// MemoryViolationRate is the fraction of samples whose working-set demand
	// exceeded the effective limit.
	MemoryViolationRate float64 `json:"memory_violation_rate"`
	// OOMKills is the number of distinct kill episodes under the cooldown model.
	OOMKills int `json:"oom_kills"`
	// OOMKillsPerDay normalises the count by trace length, so that traces of
	// different durations are comparable.
	OOMKillsPerDay float64 `json:"oom_kills_per_day"`
	// TimeToFirstOOM is how long the workload survived. Zero duration with
	// OOMKills > 0 means it failed immediately.
	TimeToFirstOOM *time.Duration `json:"time_to_first_oom,omitempty"`
	// MemoryUtilization is mean working-set demand over the request.
	MemoryUtilization float64 `json:"memory_utilization"`
	// MemoryMaxOvershootBytes is the largest excursion above the limit.
	MemoryMaxOvershootBytes float64 `json:"memory_max_overshoot_bytes"`

	// --- Provisioning quality relative to ground truth ---

	// CPUOverProvisionRatio is request / max demand. Above 1 means the
	// configuration covers every observed demand sample; below 1 means it does
	// not. It is reported as a ratio rather than a difference so that workloads
	// of different scales can be pooled.
	CPUOverProvisionRatio float64 `json:"cpu_over_provision_ratio"`
	// MemoryOverProvisionRatio is the same for memory.
	MemoryOverProvisionRatio float64 `json:"memory_over_provision_ratio"`

	Samples int `json:"samples"`
}

// Replay subjects a demand trace to a candidate configuration and reports the
// consequences.
//
// cpuRequestMilli and memoryRequestBytes are the configuration under test. A
// zero or negative value means "unconstrained" for that resource, which is used
// when a recommendation was INSUFFICIENT_DATA and no configuration was proposed.
func Replay(tr Trace, cpuRequestMilli, memoryRequestBytes float64, cfg Config) Outcome {
	out := Outcome{
		CPURequestMilli:    cpuRequestMilli,
		MemoryRequestBytes: memoryRequestBytes,
		Samples:            tr.CPUDemand.Len(),
	}
	step := tr.Spec.Step.Seconds()

	// --- CPU ---
	cpuVals := tr.CPUDemand.Values()
	if len(cpuVals) > 0 {
		var violations int
		var servedSum, throttledCoreSeconds, demandedCoreSeconds float64
		for _, d := range cpuVals {
			demandedCoreSeconds += d / 1000.0 * step
			served := d
			if cpuRequestMilli > 0 && cfg.CPURequestIsCeiling && d > cpuRequestMilli {
				served = cpuRequestMilli
				violations++
				shortfall := d - cpuRequestMilli
				throttledCoreSeconds += shortfall / 1000.0 * step
				if shortfall > out.CPUMaxShortfallMilli {
					out.CPUMaxShortfallMilli = shortfall
				}
			} else if cpuRequestMilli > 0 && d > cpuRequestMilli {
				// Request is not a ceiling: demand is served, but the excursion
				// above the request is still recorded as a violation, because it
				// is the event a throttling-based risk model would flag.
				violations++
				shortfall := d - cpuRequestMilli
				if shortfall > out.CPUMaxShortfallMilli {
					out.CPUMaxShortfallMilli = shortfall
				}
			}
			servedSum += served
		}
		out.CPUViolationRate = float64(violations) / float64(len(cpuVals))
		out.CPUThrottledCoreSeconds = throttledCoreSeconds
		if demandedCoreSeconds > 0 {
			out.CPUThrottleFraction = throttledCoreSeconds / demandedCoreSeconds
		}
		if cpuRequestMilli > 0 {
			out.CPUUtilization = servedSum / float64(len(cpuVals)) / cpuRequestMilli
			if tr.GroundTruth.CPUMaxMilli > 0 {
				out.CPUOverProvisionRatio = cpuRequestMilli / tr.GroundTruth.CPUMaxMilli
			}
		}
	}

	// --- Memory ---
	memVals := tr.MemoryDemand.Values()
	if len(memVals) > 0 {
		limit := 0.0
		if cfg.MemoryLimitEqualsRequest {
			limit = memoryRequestBytes
		}
		var violations int
		var sum float64
		var lastKill time.Duration = -1 << 62
		for i, d := range memVals {
			sum += d
			if limit > 0 && d > limit {
				violations++
				overshoot := d - limit
				if overshoot > out.MemoryMaxOvershootBytes {
					out.MemoryMaxOvershootBytes = overshoot
				}
				elapsed := time.Duration(i) * tr.Spec.Step
				if elapsed-lastKill >= cfg.OOMCooldown {
					out.OOMKills++
					lastKill = elapsed
					if out.TimeToFirstOOM == nil {
						e := elapsed
						out.TimeToFirstOOM = &e
					}
				}
			}
		}
		out.MemoryViolationRate = float64(violations) / float64(len(memVals))
		if days := tr.Spec.Duration.Hours() / 24; days > 0 {
			out.OOMKillsPerDay = float64(out.OOMKills) / days
		}
		if memoryRequestBytes > 0 {
			out.MemoryUtilization = sum / float64(len(memVals)) / memoryRequestBytes
			if tr.GroundTruth.MemoryMaxBytes > 0 {
				out.MemoryOverProvisionRatio = memoryRequestBytes / tr.GroundTruth.MemoryMaxBytes
			}
		}
	}
	return out
}

// ObservedSeries produces the series a monitoring system would have recorded for
// this workload under a given deployed configuration.
//
// This is the censoring step, and it is what makes the evaluation honest. The
// engine is given ObservedSeries, never the demand trace. When the deployed
// configuration is generous (the over-provisioned baseline the study starts
// from) observation is close to demand; when it is tight, observation
// understates demand in exactly the way a real cluster would, and the engine has
// to cope with that — which is the situation an iterative right-sizing loop
// finds itself in on its second pass.
func ObservedSeries(tr Trace, cpuRequestMilli, memoryLimitBytes float64, cfg Config) (cpu, mem model.Series) {
	cpuSamples := make([]model.Sample, tr.CPUDemand.Len())
	for i, s := range tr.CPUDemand.Samples {
		v := s.Value
		if cpuRequestMilli > 0 && cfg.CPURequestIsCeiling && v > cpuRequestMilli {
			v = cpuRequestMilli
		}
		cpuSamples[i] = model.Sample{Timestamp: s.Timestamp, Value: v}
	}
	memSamples := make([]model.Sample, tr.MemoryDemand.Len())
	for i, s := range tr.MemoryDemand.Samples {
		v := s.Value
		if memoryLimitBytes > 0 && v > memoryLimitBytes {
			// A working set above the limit is never recorded: the container is
			// killed at that point. The last observable value is the limit.
			v = memoryLimitBytes
		}
		memSamples[i] = model.Sample{Timestamp: s.Timestamp, Value: v}
	}
	return model.Series{Resource: model.ResourceCPU, Samples: cpuSamples, Step: tr.Spec.Step},
		model.Series{Resource: model.ResourceMemory, Samples: memSamples, Step: tr.Spec.Step}
}

// AsWorkload packages a trace as the model.Workload the engine consumes, with
// usage series censored by the deployed (declared) configuration.
//
// evidence is synthesised by replaying the declared configuration: if the
// workload was already being OOMKilled at its current size, the engine must see
// that evidence, because that is what a real cluster would report and it is what
// the OOM protection gate exists to react to.
func AsWorkload(tr Trace, cfg Config) model.Workload {
	declaredLimit := 0.0
	if cfg.MemoryLimitEqualsRequest {
		declaredLimit = tr.Spec.DeclaredMemory
	}
	cpuSeries, memSeries := ObservedSeries(tr, tr.Spec.DeclaredCPU, declaredLimit, cfg)
	baseline := Replay(tr, tr.Spec.DeclaredCPU, tr.Spec.DeclaredMemory, cfg)

	cpuLimit := model.Millicores(tr.Spec.DeclaredCPU)
	memLimit := model.Bytes(tr.Spec.DeclaredMemory)
	w := model.Workload{
		Namespace:     "experiment",
		Name:          tr.Spec.Name,
		Kind:          model.KindSynthetic,
		Replicas:      1,
		WorkloadClass: string(tr.Spec.Class),
		Containers: []model.Container{{
			Name: "app",
			Declared: model.ResourceRequests{
				CPURequest:    model.Millicores(tr.Spec.DeclaredCPU),
				CPULimit:      &cpuLimit,
				MemoryRequest: model.Bytes(tr.Spec.DeclaredMemory),
				MemoryLimit:   &memLimit,
			},
			CPU:    cpuSeries,
			Memory: memSeries,
		}},
		Evidence: map[string]model.RestartEvidence{
			"app": {
				Restarts:          baseline.OOMKills,
				OOMKills:          baseline.OOMKills,
				LastOOM:           lastOOMTimestamp(tr, baseline),
				CPUThrottledRatio: baseline.CPUViolationRate,
			},
		},
	}
	return w
}

func lastOOMTimestamp(tr Trace, o Outcome) *time.Time {
	if o.OOMKills == 0 || tr.MemoryDemand.Len() == 0 {
		return nil
	}
	// The engine's OOM gate checks recency against wall-clock now, so the
	// synthetic timestamp is anchored to the end of the trace: a trace always
	// represents the window that just elapsed.
	t := time.Now().Add(-tr.Spec.Step)
	return &t
}

// SavingsFraction is the relative reduction from a to b, guarding against a
// zero baseline. It is defined here so that the simulator and the experiment
// runner cannot disagree about its sign convention: positive means cheaper.
func SavingsFraction(from, to float64) float64 {
	if from <= 0 {
		return 0
	}
	return (from - to) / from
}

// Clamp01 constrains a fraction to [0,1] for reporting. It is applied only to
// quantities that are fractions by construction, where a value outside the range
// indicates floating-point drift rather than a real measurement.
func Clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }
