# Architecture

## Shape of the system

```
                         Kubernetes API (client-go)
                                    │
                     discovery: workloads, declared requests,
                     controller ownership, restart/OOM evidence
                                    │
   Prometheus ──────────────────────┤
   usage history                    │
                                    ▼
                       ┌────────────────────────┐
                       │     model.Series       │◄──── internal/simulator
                       │  (the single boundary) │      (research mode)
                       └───────────┬────────────┘
                                   ▼
                        Recommendation engine
                  ┌────────────────┴────────────────┐
                  │  Strategy   — pure statistics   │
                  │  Gate chain — safety rules      │
                  └────────────────┬────────────────┘
                   ┌───────────────┴───────────────┐
                   ▼                               ▼
             Cost estimate                  Risk classification
                   └───────────────┬───────────────┘
                                   ▼
                    REST API  ·  CLI  ·  Prometheus metrics
```

## The decisions that shaped it

### A modular monolith, not microservices

The controller and API server run in one process. Splitting them would require a
shared store and a synchronisation protocol to solve a problem that does not
exist: the API serves a snapshot the controller produces, in memory, with a
read-write mutex.

The cost of this choice is that the API cannot scale independently of analysis.
That would begin to matter at a request rate far above what a dashboard and a
handful of engineers generate. Until then, the operational simplicity of one
binary, one deployment and one failure domain is worth more.

### `model.Series` is the only boundary that matters

Data acquisition and analysis meet at exactly one type. In production
`internal/promapi` produces it from a Prometheus range query; in research
`internal/simulator` produces it from a generative model. Nothing downstream can
tell the difference.

This is what allows the experimental findings to be read as statements about the
deployed system. If research mode ran a separate recommender, the experiments
would be measuring something that is not shipped.

### Strategies are pure; gates hold the evidence

```go
type Strategy interface {
    Target(s stats.Summary) float64   // statistics only
}

type Gate interface {
    Evaluate(in GateInput) GateResult // may see evidence and the current request
}
```

A `Strategy` may not consult the current request or reliability evidence. That
restriction keeps the experimental baselines honest: a "strategy" that could see
the current request could trivially never regress, and would no longer be a
baseline.

Gates then apply safety rules that *do* need that context — OOM history, data
sufficiency, how far the target is from what is deployed.

### The gate chain has one invariant

```
result.Target  >=  min(proposed, current)
```

A gate may hold the line, raise a target, or cancel a proposed change; it may
never produce a configuration more aggressive than both the strategy's proposal
and what is already running. The invariant is asserted at runtime and in tests.

The `min()` rather than the proposal alone is deliberate and was learned the hard
way: cancelling a marginal *increase* legitimately lowers the target back to the
current request. A stricter version of this assertion fired mid-experiment on a
9% proposed increase, which is the intended behaviour of the minimum-change gate.

Gate order is also not arbitrary:

1. **data sufficiency** — if the data cannot support a decision, nothing else applies
2. **OOM protection** — the hardest safety constraint, before any cosmetic rule
3. **usage exceeds request** — a workload already at its request is not over-provisioned
4. **instability** — erratic series make percentile estimates unreliable
5. **floor** — raise implausibly small targets
6. **minimum change** — last, so it sees the final target and can collapse a marginal change to NO_CHANGE

### Advisory by default, in three independent layers

1. `analysis.apply` defaults to false.
2. `rbac.allowApply` defaults to false, so the service account has no write verbs.
3. `BLOCKED` and `INSUFFICIENT_DATA` are never applied even in apply mode.

Any one of these alone would prevent accidental mutation. All three are present
because the failure mode — restarting every pod in a production workload with a
request that is too small — is severe and silent until it is not.

### Snapshots, not request-time computation

The API serves the most recent completed analysis. Computing per request would
issue a burst of Prometheus queries on every dashboard refresh, turning a
monitoring dashboard into a load generator against the monitoring system it
depends on.

The cost is staleness bounded by `analysis.interval`, which is why
`optimizer_last_analysis_timestamp_seconds` exists and why the dashboard shows
staleness prominently: recommendations served after a long gap describe a cluster
that no longer exists.

## Package layout

| Package | Responsibility |
|---|---|
| `internal/model` | Domain types: `Series`, `Workload`, `Recommendation`. No dependencies on anything else in the project. |
| `internal/stats` | Percentiles, summaries, bootstrap intervals. Deliberately free of domain types. |
| `internal/recommender` | Strategies, gates, the engine, the policy. The core. |
| `internal/cost` | Allocation-based cost model and instance price catalog. |
| `internal/kube` | Discovery, owner resolution, OOM evidence, patching. |
| `internal/promapi` | Prometheus queries. The only place PromQL appears. |
| `internal/simulator` | Generative workloads, censoring, counterfactual replay. |
| `internal/experiment` | Matrix expansion, provenance, result records. Contains no right-sizing logic. |
| `internal/api` | Analysis service and HTTP handlers. |
| `internal/config` | Configuration loading and validation. |
| `internal/metrics` | The optimizer's own Prometheus instrumentation. |
| `pkg/quantity` | Kubernetes quantity conversion. The only place rounding happens. |
| `pkg/humanize` | Durations that round-trip through YAML readably. |

`internal/experiment` containing no right-sizing logic is a property worth
protecting: if it did, the experiments would be measuring code that is not
deployed.

## Scaling

The current design is tested at tens of workloads. What follows is a *design*,
not an implementation, and no performance claim is made about it.

| Workloads | Per cycle | Status |
|---|---|---|
| 10 | ~20 Prometheus queries | measured; sub-second |
| 100 | ~200 queries | expected to work as built |
| 1,000 | ~2,000 queries | needs concurrency tuning and a longer interval |
| 10,000 | ~20,000 queries | needs recording rules |
| 100,000 | — | needs sharding |

**The binding constraint is Prometheus query volume, not optimizer CPU** — and
that is a measurement rather than an assumption:

```
$ make bench
BenchmarkRecommend/7day-10        4786     252338 ns/op    330576 B/op    56 allocs/op
BenchmarkSummarize/7day-10        4482     239456 ns/op     81952 B/op     2 allocs/op
```

A full recommendation for one workload over a 7-day window at 1-minute resolution
costs **252 µs**. Ten thousand workloads is therefore ~2.5 CPU-seconds per cycle —
negligible against a 15-minute interval.

The same ten thousand workloads require **30,000 Prometheus range queries** (CPU,
memory and throttling per container), each returning ~10,000 samples. That is
where the time and the risk go.

The batched-percentile path is worth its complexity for the same reason it is not
the bottleneck — one sort serving four quantiles measures 175 µs against 698 µs
for four separate calls:

```
BenchmarkPercentilesVersusIndividual/batched-10       6110    174788 ns/op
BenchmarkPercentilesVersusIndividual/individual-10    1711    697941 ns/op
```

(Measured on an Apple M5, `go test -bench`. Absolute figures will differ by
machine; the ratio between compute and query cost is the durable part.)

What would have to change, in the order it would become necessary:

1. **Recording rules.** Precompute per-container aggregates in Prometheus so the
   optimizer reads one cheap series instead of aggregating at query time. This is
   the single highest-leverage change and would likely carry the design to ~10,000
   workloads on its own.
2. **Informers instead of list calls.** Replace periodic `List` with shared
   informers and a watch, so discovery cost becomes proportional to churn rather
   than to cluster size.
3. **Staggered analysis.** Analyse a fraction of workloads per cycle rather than
   all of them, spreading query load. Recommendations change slowly; a workload
   analysed every hour instead of every 15 minutes loses little.
4. **Sharding by namespace.** Horizontal partitioning with a coordinator for the
   cluster-wide summary. Only at this point does the monolith stop being adequate,
   which is why it is last.
5. **Persistence.** Snapshots are in memory today. At scale, a store would allow
   recommendation history, drift detection and restart without re-derivation.

Deliberately absent: a message queue, a service mesh, a database. None of them
addresses the actual constraint, which is the number of range queries issued
against Prometheus.

## Failure modes

See [failure-modes.md](failure-modes.md) for the full table. The design
principle: **degrade to silence, never to a confident wrong answer.** When the
engine cannot support a decision it reports `INSUFFICIENT_DATA`; when evidence is
missing it withholds reductions; when Prometheus is unreachable it serves the
last snapshot and lets staleness metrics surface the problem, rather than
recomputing from nothing.
