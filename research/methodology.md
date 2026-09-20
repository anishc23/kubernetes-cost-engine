# Methodology

## The central difficulty: measurement is censored by the thing being measured

A right-sizing recommendation is a prediction: *this request will be enough*. To
evaluate it, one needs to know what the workload actually needed. On a real
cluster that is unobservable, and unobservable in a way that is biased rather
than merely noisy.

Consider evaluating a CPU recommendation by checking measured usage after
applying it. If the request was too low, the container was throttled, so the
measured usage is *at* the request, not above it. The evaluation reports perfect
utilisation and no violation — for a configuration that degraded the service. The
same applies to memory with more force: a container killed at its limit never
records the working set it was reaching for, so the series tops out exactly at the
limit that killed it.

**Measured usage is censored by the configuration under test, and it is censored
most severely in precisely the cases where the configuration is wrong.** Comparing
a recommendation against measured usage therefore cannot distinguish "the
recommendation was correct" from "the recommendation suppressed the evidence that
would have falsified it".

Every design decision below follows from taking that problem seriously.

## Approach: generative ground truth with counterfactual replay

The evaluation uses synthetic workloads whose demand is generated from a known
model, which separates two things a real cluster conflates:

- **Demand** — what the workload wants, independent of any configuration.
- **Observation** — what a monitoring system would have recorded, which is demand
  after the deployed configuration has interfered with it.

The engine is given only the observation. It is scored against the demand.

```
  generative model
        |
        v
   demand trace  ──────────────────────────────┐
        |                                      │
        | censor by the deployed configuration │  (never shown to the engine)
        v                                      │
  observed series                              │
        |                                      │
        v                                      │
  recommendation engine  (production code)     │
        |                                      │
        v                                      │
   recommendation ─────> replay against demand <┘
                              |
                              v
                    throttling, OOMKills, cost
```

The replay step (`internal/simulator/replay.go`) subjects the demand trace to the
recommended configuration and derives what would actually have happened. This is a
counterfactual the engine had no access to, which is what makes it a test rather
than a restatement.

### What this buys, and what it costs

It buys exact ground truth: the required resource envelope is known, not
estimated. It costs external validity: synthetic demand is not production demand,
and a policy tuned on these classes has not been shown to work on real ones. That
limitation is treated as a first-class threat in
[threats_to_validity.md](threats_to_validity.md), not as a footnote.

The production path is validated separately and by different means: a `kind`
cluster runs a real load generator whose metrics travel the real
cAdvisor → Prometheus → optimizer path (`test/e2e`). That confirms the collection
machinery works but provides no ground truth. The two methods are complementary,
and [experimental_setup.md](experimental_setup.md) states which results come from
which.

## Train/test separation

Each trace is split into a **fitting window**, which the engine may observe, and a
**held-out evaluation horizon**, which it may not:

```
|<----------- fitting window ----------->|<--- evaluation horizon --->|
            (engine sees this,                  (engine never sees this;
             censored)                           recommendation scored here)
```

Scoring on the fitting window would measure how well a percentile describes the
data it was computed from, which is arithmetic rather than a result. Holding out
the horizon makes the evaluation a genuine prediction task.

The horizon is anchored at the **end** of the trace, so that every observation
window length is scored against the same future. Without that anchoring, the
window comparison (RQ6) would be comparing different futures rather than different
histories.

Implemented in `splitTrace` (`internal/experiment/runner.go`), with non-overlap
asserted by `TestFitAndEvaluationWindowsDoNotOverlap`.

## Replay semantics, and why they are pessimistic

Two modelling choices determine what "failure" means. Both were chosen to avoid
flattering the engine.

**CPU request treated as a ceiling.** In Kubernetes a CPU request is a
`cpu.shares` weight, not a cap; on an idle node a container freely exceeds it, and
only a CPU *limit* imposes a hard CFS quota. The replay treats the request as the
amount obtainable, which models a fully contended node.

The alternative was rejected on methodological grounds, not conservatism: if
containers can always burst above their request, CPU under-provisioning has no
measurable consequence, every strategy scores identically on CPU risk, and RQ2
becomes unanswerable. Reporting under contention is the assumption under which the
CPU comparison carries information. The consequence — reported CPU violation rates
are an upper bound on what a well-provisioned cluster would experience — is stated
in [threats_to_validity.md](threats_to_validity.md).

**Memory limit set equal to the recommended request.** Without a limit a container
is killed only under node-level pressure, which depends on co-tenancy the model
does not represent. Setting limit = request (the Guaranteed QoS pattern) gives a
well-defined pessimistic bound: any excursion above the recommendation is fatal.
Results are exact for Guaranteed pods and an upper bound on OOM risk for Burstable
ones.

**OOMKill counting.** A sustained excursion above the limit is one sizing failure,
not one per sample. Kills are counted with a two-minute cooldown, approximating
crash-loop backoff, so the metric counts distinct kill–restart episodes rather than
measuring excursion length. Verified by `TestOOMCooldownCountsEpisodesNotSamples`.

## Metric definitions

All metrics are computed from ground-truth demand over the held-out horizon.

### Cost

| Metric | Definition |
|---|---|
| `savings_fraction` | (current cost − recommended cost) / current cost. Negative for increases, reported rather than clipped. |
| `monthly_savings_usd` | Allocation-based monthly difference, at the configured rate. |

Cost is an **allocation-based estimate**: it prices reserved capacity, not cloud
invoices. Its limits are documented in [docs/cost-model.md](../docs/cost-model.md)
and it is an upper bound on achievable savings.

### Efficiency

| Metric | Definition |
|---|---|
| `cpu_utilization` | mean served CPU demand / CPU request. |
| `memory_utilization` | mean working-set demand / memory request. |

### Risk

| Metric | Definition | Why both |
|---|---|---|
| `cpu_violation_rate` | fraction of samples with demand > request | how *often* degradation occurred |
| `cpu_throttle_fraction` | unmet CPU-seconds / demanded CPU-seconds | how *much* was degraded |
| `oom_kills` | distinct kill episodes under the cooldown model | discrete failure count |
| `survived_without_oom` | zero OOMKills over the horizon | the outcome operators care about |

Frequency and magnitude are reported separately because a policy can be good at
one and bad at the other: being 1% short for many samples and 400% short once are
very different failures, and a single metric conflates them.
`TestViolationRateAndThrottleFractionAreDistinct` pins that they can diverge.

### Accuracy against ground truth

| Metric | Reference | Rationale |
|---|---|---|
| `cpu_relative_error` | true p99 demand | sizing every workload to its single largest sample is not the operating point teams choose; error should be measured against a realistic target |
| `memory_relative_error` | true **maximum** demand | for memory the maximum *is* the target: anything below it is a kill |

The different references are not an inconsistency. They are the CPU/memory
asymmetry expressed in the measurement, and using one reference for both would
build the assumption being tested into the instrument.

### The constrained objective

```
feasible  ⟺  oom_kills ≤ 0  AND  cpu_throttle_fraction ≤ 0.01

feasible_savings = savings_fraction if feasible else 0
```

The asymmetry between the two thresholds is deliberate: one OOMKill is a failure
regardless of savings, while 1% of demanded CPU work unserved is a normal
operating point. Both thresholds are varied in the sensitivity analysis.

### Stability (RQ7)

```
volatility = median | log2( r_t / r_{t-1} ) |
```

over successive scheduled recomputations. The log-ratio is used because it is
symmetric: a doubling and a halving are the same magnitude of change, which a
percentage difference gets wrong (+100% versus −50%). Zero means perfectly stable;
1.0 means the typical recomputation doubled or halved the request. Absent
recommendations (`INSUFFICIENT_DATA`) are skipped rather than treated as a swing to
zero, since declining to answer is not a volatile answer.

## Statistical treatment

**Aggregation.** Medians with 95% percentile-bootstrap intervals over 10
independent trace realisations per condition. The bootstrap is used because the
per-condition distributions are not symmetric — savings are bounded above by 1,
throttle fractions are bounded below by 0 with a long right tail, and OOM counts
are zero-inflated. A mean with a normal-theory interval would assume away exactly
that structure and would produce intervals extending below zero for quantities
that cannot be negative.

**Proportions.** Feasibility and survival rates use Wilson score intervals, which
behave correctly near 0 and 1 where the normal approximation produces bounds
outside [0,1].

**No significance tests are reported, deliberately.** With ten seeds per condition
and thousands of conditions, a p-value would be underpowered per comparison and
hopelessly multiple-compared across them. More fundamentally, the seeds are draws
from a generative model chosen by the author: a "significant" difference would be
a statement about the simulator's parameters, not about workloads. Effect sizes
with intervals answer the question that can actually be answered — how large is
the difference, and how much does it move across realisations.

**Tail reporting.** Where a median is zero for most conditions, the 90th
percentile is reported alongside it. A risk metric that is zero almost everywhere
cannot show the shape of the tail that matters, and reporting only its median
would understate risk by construction.

## Reproducibility

Every result is regenerable from the repository:

```bash
make experiments   # regenerate all result files
make analysis      # regenerate every figure and table
```

Each run records provenance: git commit and whether the tree was dirty, a content
hash of the resolved configuration, the tool version, Go version, OS and
architecture, the cost model, and timestamps. Traces are seeded per workload class
with a prime stride, so adding a class does not perturb the traces of existing
ones — without that property, extending the study would silently change every
previously published result (`TestSeedsAreIndependentAcrossClasses`).

Ablation configurations share their base seed and trace geometry with the
experiment they are compared against, so a difference between them is attributable
to the ablated component rather than to a different draw of traces
(`TestAblationConfigsShareSeedsWithMain`).

Parallel execution is verified not to change results
(`TestParallelismDoesNotChangeResults`); conditions are independent by
construction, so concurrency affects only wall-clock time.

## Research mode and production mode share one engine

The experiments invoke `recommender.Engine` — the same type, through the same
entry point, that the deployed controller uses. The only difference is the data
source: `internal/promapi` in production, `internal/simulator` in research, both
producing `model.Series`. Nothing downstream can tell them apart.

This is what licenses reading the findings as statements about the deployed
system. If the experiments exercised a separate research prototype, they would be
statements about that prototype instead.
