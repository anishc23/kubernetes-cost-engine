# Failure Modes

**Design principle: degrade to silence, never to a confident wrong answer.**

When the engine cannot support a decision it says so. It does not fall back to a
default, extrapolate from partial data, or report a recommendation it cannot
justify. Every entry below follows from that.

## Dependencies

### Prometheus unreachable

**Symptom:** `optimizer_prometheus_query_errors_total{reason="unreachable"}` rises;
`/readyz` returns 503; the analysis cycle fails.

**Behaviour:** The previous snapshot is retained and continues to be served.
Liveness (`/healthz`) keeps passing, deliberately — a liveness probe that failed
here would restart a healthy optimizer and turn a dependency outage into a crash
loop, making recovery slower.

**Why not clear the snapshot:** stale recommendations with a visible timestamp are
more useful than none. The staleness metric is the control.

**Detect:** alert on `time() - optimizer_last_analysis_timestamp_seconds` exceeding
a few analysis intervals.

### Prometheus reachable but has no data

**Symptom:** every workload reports `INSUFFICIENT_DATA`.

**Cause:** usually the wrong Prometheus address, cAdvisor not being scraped, or
metric relabelling that dropped `container_cpu_usage_seconds_total`.

**Behaviour:** the sufficiency gate blocks every change and states the sample
count and coverage in its reason, so the cause is visible in the API rather than
requiring a log dive.

**Detect:** `optimizer_prometheus_samples_returned` collapsing toward zero, and
`optimizer_gates_fired_total{gate="data-sufficiency"}` rising.

### Prometheus returns partial data (gaps)

**Symptom:** series span the window but contain far fewer samples than expected.

**Behaviour:** the coverage check catches this. A series spanning seven days with
3% of its expected samples is reported as insufficient rather than summarised as
though it described the period.

**Why it matters:** without a coverage check, a series that lost 90% of its
samples during an incident would still produce a confident percentile — computed
entirely from the quiet periods that remained.

### Kubernetes API unreachable

**Behaviour:** discovery fails, the cycle aborts, the previous snapshot is served.
A single *namespace* failing (RBAC, transient error) does not abort the pass:
partial results with a logged error are more useful than none, and RBAC commonly
grants access to a subset.

**Detect:** `optimizer_kubernetes_api_errors_total`.

### Prometheus query ambiguity

**Symptom:** a query returns more than one series.

**Behaviour:** the workload fails with an explicit error naming the problem.
Returning the first series would silently analyse an arbitrary subset of the
workload's pods.

## Data quality

### Workload has OOM history

**Behaviour:** memory reductions are blocked; `BLOCKED` with the OOM count in the
reason; risk `HIGH`. CPU is still right-sized — an OOM is not evidence about CPU.

**Why:** the working-set series of an OOMKilled container is censored at the limit
and understates true demand. The statistics say "you are using less than you
reserved" precisely because the process keeps dying before it can use more.

### No restart or OOM evidence could be collected

**Behaviour:** memory reductions are blocked. Absent evidence is not evidence of
absence.

**When this happens:** a workload whose pods have all been recreated recently, or
a namespace where pod listing is denied.

### Newly deployed workload

**Behaviour:** `INSUFFICIENT_DATA` until the sample, duration and coverage
thresholds are met.

**Why:** the first minutes of a container's life are its least representative —
JIT warm-up, cache filling, connection pool establishment. Sizing from them would
produce a request that fails as soon as the workload settles.

### Extremely bursty workload

**Behaviour:** above the burstiness threshold, CPU reductions are withheld with
`BLOCKED` and a reason quoting the measured ratio.

**Why:** a percentile computed over the window does not describe the peaks such a
workload reaches. Measured: on the spiky class, even p99 left 8.5% of demanded work
unserved.

### Usage already exceeds the request

**Behaviour:** reductions blocked. The workload is not over-provisioned in any
sense that justifies one, and for CPU this indicates it has been competing for
time above its request.

## Cluster events

### Node failure or pod eviction during the window

**Behaviour:** the affected pods' series end. Coverage drops, and if it falls below
the threshold the workload reports `INSUFFICIENT_DATA` rather than being sized
from the surviving fragment.

### Workload scaled or deleted between cycles

**Behaviour:** the next cycle rediscovers from scratch; deleted workloads simply
disappear from the snapshot. There is no cached state to become inconsistent.

### Replica count changes

**Behaviour:** cost is recomputed from the current replica count. Recommendations
themselves are per-container and unaffected, since the request lives in the pod
template.

## The optimizer itself

### Crash or restart

**Behaviour:** the snapshot is in memory and is lost. The first cycle runs
immediately on startup rather than after one interval, so the gap is seconds
rather than minutes. `/readyz` reports `analysis: pending` until it completes, so
the Service does not route to a pod that would answer with an empty result set.

**Consequence:** no recommendation history survives a restart. Recommendation
drift over weeks is not observable in production the way it is in the research
framework.

### Analysis slower than the interval

**Behaviour:** cycles are sequential, not concurrent — the ticker fires into a
loop that is still working, and the tick is dropped. Cycles cannot pile up.

**Detect:** `optimizer_analysis_duration_seconds` approaching `analysis.interval`.
The fix is a longer interval or Prometheus recording rules, not more concurrency:
the constraint is query volume.

### Invalid configuration

**Behaviour:** validation runs before any component is constructed, and reports
**all** problems at once rather than one per restart. A safety factor below 1.0 is
rejected rather than clamped, because it is more likely a mistake than an
intention and silently correcting it would hide the mistake.

The Helm chart additionally fails at render time for the dangerous cases, so they
never reach a cluster.

### Clock skew

**Behaviour:** affects OOM recency checks (`oomLookbackRelevance`) and the
staleness metric. Large skew could make a recent OOM appear stale and so permit a
memory reduction the gate should have blocked.

**Mitigation:** none in-process; this relies on node time synchronisation. Noted
rather than solved, since a tool cannot fix its host's clock.

### Apply mode applies something harmful

**Behaviour, in layers:** mutation is off by default and requires two independent
settings; only requests are patched, never limits; `BLOCKED` and
`INSUFFICIENT_DATA` are never applied; reductions are bounded by
`applyMaxDecreaseFraction`; the `delete` verb is never granted.

**If it happens anyway:** `kubectl rollout undo` restores the previous pod
template. Keep apply mode scoped to a namespace until you trust it.

## What is not handled

Stated plainly rather than left to be discovered:

- **Prometheus returning wrong data** (misconfigured relabelling that silently maps
  one workload's metrics to another). The engine cannot detect this; it would
  produce confident, wrong recommendations.
- **Concurrent writers.** Running alongside the Vertical Pod Autoscaler in apply
  mode means two controllers writing the same field. The engine does not detect VPA
  ownership.
- **Multi-cluster.** One optimizer, one cluster.
- **Recommendation history.** No persistence, so no drift detection in production.
