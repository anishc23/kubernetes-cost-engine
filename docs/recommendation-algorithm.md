# The Recommendation Algorithm

## The systems fact everything rests on

Kubernetes treats CPU and memory differently at the kernel level, and the
difference is not a matter of degree.

**CPU is compressible.** A request becomes a `cpu.shares` weight; a limit becomes
a CFS quota. A container wanting more CPU than it can get is *delayed* — throttled
into the next scheduling period. Latency rises. The process lives. Raising the
request fixes it.

**Memory is incompressible.** There is no way to give a process less memory than
it allocates. When a container's working set exceeds its limit, the kernel OOM
killer terminates it. The process dies. The pod restarts. In-flight requests are
lost.

A system that applies one formula to both resources has decided that a latency
increase and a process kill are the same kind of event. They are not, and the
experiments in [research/results.md](../research/results.md) show the two
resources fail on *different workloads*, not merely to different degrees.

## The pipeline

```
observed series ──► statistics ──► strategy ──► gate chain ──► rounding ──► decision
                    (percentiles)   (target)    (safety)       (grid)       (+reason)
```

### 1. Statistics

Mean, standard deviation, p50/p90/p95/p99, maximum, and burstiness (max/mean),
computed over the observation window.

Percentile estimation is explicit rather than incidental. Two estimators are
offered:

- **Linear interpolation** (R-7, the numpy default) — the shipped default,
  because it is what a reader reproduces with `numpy.percentile`, which matters
  when the analysis layer is Python.
- **Nearest rank** — always returns a value the workload actually reached. For
  safety-critical sizing, an interpolated value that was never observed is harder
  to defend.

They diverge only on short windows, where a p99 over few samples interpolates
between the two largest observations. Measured effect: ~4 percentage points of
savings at a 1-hour window, immaterial beyond 6 hours.

### 2. Strategy

A pure function from statistics to a target:

```
target = statistic(observed) × safety_factor
```

Available: `mean`, `p50`, `p90`, `p95`, `p99`, `max`, and `current` (the
unchanged-request baseline). A strategy may not see the current request or
reliability evidence — that restriction is what keeps the experimental baselines
comparable.

### 3. Gates

Applied in order. Each may raise a target or cancel a change; none may go below
`min(proposed, current)`.

| Gate | What it does | Why |
|---|---|---|
| **data sufficiency** | blocks all changes below a sample/duration/coverage threshold | a p99 over three samples is arithmetic applied to noise; coverage catches series that span the window but are mostly gaps |
| **OOM protection** | blocks memory *reductions* for containers with recent OOM history, and when no evidence was collected at all | an OOMKilled container's working-set series is censored at the limit and understates true demand; absent evidence is not evidence of absence |
| **usage exceeds request** | blocks reductions when observed p99 already meets the request | such a workload is not over-provisioned in any sense that justifies a reduction |
| **instability** | blocks CPU reductions above a burstiness threshold | above it, a percentile over the window does not describe the peaks the workload reaches |
| **floor** | raises targets below an absolute minimum | a 1m CPU request is below the granularity at which CFS enforces quota and leaves no headroom for startup |
| **minimum change** | collapses sub-threshold changes to NO_CHANGE | applying a 3% change restarts every pod; a tool that emits such advice trains operators to ignore it |

OOM protection never blocks an *increase*. The gate exists to prevent unsafe
reductions, not to freeze a workload that needs more memory.

### 4. Rounding

Always upward, never downward. The safety factor has already been applied, so
rounding down would erode it.

- **CPU to 10m.** The CFS quota period is 100ms by default, so 10m is 1ms of quota
  per period. Finer values are below the resolution at which the kernel enforces
  the request, and emitting them implies precision the system does not have.
- **Memory to 1 MiB.** The smallest unit operators write; sub-MiB precision is
  noise relative to page cache and allocator behaviour.

### 5. Decision

| Decision | Meaning |
|---|---|
| `DECREASE` / `INCREASE` | act |
| `NO_CHANGE` | a target was computed and is not materially different |
| `BLOCKED` | a gate refused a reduction the statistics would have supported |
| `INSUFFICIENT_DATA` | not enough data to make any claim; carries no target |

`NO_CHANGE` and `BLOCKED` are distinct on purpose. Collapsing them would hide
exactly the cases an operator most needs to see. `BLOCKED` recommendations still
report `raw_target` — the value the statistics produced before the gate — so the
gate's effect is measurable rather than invisible.

## The shipped defaults, and why they changed

```yaml
cpu:    p99 × 1.25, with reductions withheld above 20x burstiness
memory: max × 1.25
```

**These are experimental results, not preferences.**

The project originally shipped `p95 × 1.15` for CPU, on the reasoning that CPU
degradation is recoverable and therefore tolerates a lower percentile. The
reasoning is sound. Measured against held-out demand, the default left **34% of
demanded CPU work unserved** on bursty workloads.

The replacement was derived from the same data:

- p99 at 1.25× is safe on every evaluated class except the spiky one, whose peaks
  are rarer than 1% of samples and therefore sit above p99 by construction.
- Observed burstiness separates that class cleanly: it measures 38–43, while every
  other class — including the bursty one at 13.0 — measures below 14.
- An instability threshold of 20 therefore withholds the reduction exactly where
  p99 fails.

Confirmed in a dedicated run: 100% feasibility across every class and seed, at
33.7% median savings.

**The cost is real and is not hidden.** `p95 × 1.25` saves 58% against this
configuration's 34%, and is feasible on 80% of conditions rather than 100%. An
operator who knows their workloads are not spiky should prefer it. The default is
conservative because the engine cannot know that on their behalf.

**The burstiness threshold is the project's most overfitted parameter** — selected
on ten synthetic classes and validated on the same ten. See
[research/threats_to_validity.md](../research/threats_to_validity.md).

## Why memory uses `max` and CPU does not

Two findings, both counter-intuitive:

**The observed maximum is not an upper bound.** Sizing memory to the maximum seen
in the fitting window, with no margin, survived only **47%** of conditions. The
maximum of a finite sample estimates a distribution's upper tail; it does not
bound it. Hence the 1.25× margin.

**A margin cannot repair a statistic that excludes the event.** Memory `p95`
plateaus at 90% survival and stays there even at a 2.0× margin, while `max`
reaches 100% at 1.2×. On the bursty-memory class, p95 of the working set is
roughly 27% of its peak — doubling it still lands below the peak.

For CPU the same logic does not apply, because being below the peak means
throttling rather than death, and paying for the peak of a bursty workload means
reserving cores that sit idle 97% of the time.

## Worked example

```
Container: checkout-service, 7-day window, 1-minute resolution

CPU observed:     mean 180m, p95 340m, p99 410m, max 1250m  (burstiness 6.9)
Memory observed:  mean 680Mi, p95 720Mi, p99 735Mi, max 780Mi
Declared:         cpu 2000m, memory 2Gi
Evidence:         0 restarts, 0 OOMKills

CPU:
  strategy p99 × 1.25  →  410 × 1.25 = 512.5m
  burstiness 6.9 < 20  →  instability gate does not fire
  p99 410m < 2000m     →  usage-exceeds gate does not fire
  round up to 10m      →  520m
  change 74% ≥ 10%     →  DECREASE 2000m → 520m, risk LOW

Memory:
  strategy max × 1.25  →  780 × 1.25 = 975Mi
  evidence present, 0 OOMKills → OOM gate does not fire
  round up to 1Mi      →  975Mi
  change 52% ≥ 10%     →  DECREASE 2Gi → 975Mi, risk LOW
                          (975Mi is 1.25× the observed max: headroom ≥ 1.15 → LOW)

Estimated saving: $46.08/month at 3 replicas, m5.xlarge-derived rates
```

Now change one fact — the container was OOMKilled twice yesterday:

```
Memory:
  strategy max × 1.25  →  975Mi        (raw_target, still reported)
  OOM gate fires       →  BLOCKED at 2Gi, risk HIGH
  reason: "memory reduction withheld: 2 OOMKill(s) observed, so the working-set
           series is censored and understates true demand"

CPU is unaffected: an OOM is not evidence about CPU.
Estimated saving: $33.70/month — from CPU alone.
```

That asymmetry, in one worked example, is the whole argument.
