# Research Questions

## The problem

Kubernetes schedules workloads against *declared* resource requests, not against
what they use. The request determines how much of a node is reserved, and
therefore how many nodes a cluster needs; the usage determines how much of that
reservation is actually consumed. The difference is capacity that is paid for and
idle.

Operators over-declare for a defensible reason: the cost of under-declaring is
asymmetric and immediate. A CPU request set too low causes throttling and latency;
a memory limit set too low causes the process to be killed. The cost of
over-declaring is diffuse, deferred, and paid by a different team. Rational
behaviour at the level of a single service produces systematic waste at the level
of a cluster.

Automating the correction is harder than it appears, for a reason that is easy to
overlook: **the usage data available to a right-sizing system is not the workload's
demand**. It is demand after the cluster has already interfered with it. CPU that
was throttled was never recorded as used. A container that was OOMKilled never
recorded the working set it was reaching for. The measurement is censored by the
configuration being evaluated, and it is censored hardest in exactly the cases
where the configuration is wrong.

## Central question

> How accurately and safely can Kubernetes workload resource requests be
> right-sized from historical utilisation data, and what is the measurable
> trade-off between cost reduction and reliability risk?

"Safely" is the operative word. A system that reports a 70% saving while causing
periodic OOMKills has not solved the problem; it has moved the cost from a budget
line to an incident channel.

## Research questions

### RQ1 — How much waste is there, and does the question have a single answer?

How far do declared requests exceed true demand, and how much does that figure
depend on which demand statistic is taken as the reference?

*Why it matters:* published waste figures are usually a single headline number.
If the answer varies by an order of magnitude with the reference statistic, such
figures are not comparable between studies, and the reference must be stated
alongside any claim.

### RQ2 — How do CPU right-sizing strategies compare?

Compare mean, p50, p90, p95, p99, maximum, and percentile-with-margin against the
unchanged request, on savings, utilisation, request violations, and the fraction
of demanded CPU work left unserved.

*Falsifiable prediction:* higher percentiles are safer and cheaper in a simple
monotone trade-off, so a single percentile can be recommended for CPU generally.

### RQ3 — How do memory strategies compare, and do they rank the same way?

The same comparison for memory, scored on OOMKills and survival rather than on
degradation.

*Falsifiable prediction:* the ranking is the same as for CPU, so one policy can
serve both resources and the engine's resource-specific design is unnecessary
complexity.

### RQ4 — Does workload behaviour change the answer?

Evaluate across stable, bursty, periodic, spiky, growing, sawtooth, mixed and idle
classes, where the generative structure of each is known exactly.

*Falsifiable prediction:* strategy ranking is stable across classes, so a
workload-agnostic recommender is sufficient.

### RQ5 — What does safety cost?

Quantify the trade-off between savings, utilisation, degradation and failure as a
frontier rather than a ranking, and identify which configurations are feasible
under an explicit reliability constraint.

*Why a constrained objective, not a weighted score:* a weighted score requires
choosing how many OOMKills a dollar is worth. Nobody can defend that exchange
rate, and encoding one lets a policy trade reliability for savings at a rate no
operator agreed to. The constrained form answers the question operators actually
ask: among configurations that are acceptably safe, which saves most?

### RQ6 — How much history is needed?

Compare observation windows of 1h, 6h, 24h and 72h, with every window scored on
the same held-out future so that the comparison isolates history length.

*Falsifiable prediction:* longer windows are monotonically better, and the only
cost is staleness.

### RQ7 — Are recommendations stable enough to operate?

Measure how much a recommendation moves between scheduled recomputations.

*Why it matters:* applying a request change restarts every pod in the workload. A
recommender whose output oscillates is not deployable however accurate each
individual recommendation is. This is a systems property that accuracy metrics do
not capture, and it is rarely reported.

## What would falsify the project's central design claim

The engine treats CPU and memory with different policies because their failure
modes differ. That claim is falsified if, across the evaluated workloads:

1. the best-performing CPU strategy and the best-performing memory strategy are
   the same, **and**
2. forcing a single shared strategy (Ablation C) costs nothing measurable on
   either savings or reliability.

Both were tested. The outcome is reported in [results.md](results.md), including
the parts that did not support the project's initial configuration.

## Relationship to existing systems

This project does not claim a novel algorithm. Percentile-based right-sizing is
implemented in the Kubernetes Vertical Pod Autoscaler, surfaced by Goldilocks, and
sold by several commercial platforms. The contribution claimed here is
**empirical**: a reproducible, ground-truth evaluation of these strategies under
controlled workload classes, with the censoring problem handled explicitly and
reliability scored rather than assumed. See [related_work.md](related_work.md) for
what exists and what is genuinely different.
