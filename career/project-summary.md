# Project Summary

**Kubernetes Cost Optimization Engine** — a resource right-sizing system for
Kubernetes and a ground-truth evaluation of whether right-sizing recommendations
can be trusted.

*Everything below is backed by code in this repository or by measurements in
`experiments/results/`. No claim here is aspirational.*

---

## One paragraph

Kubernetes reserves capacity by declared request rather than usage, so clusters
carry substantial idle reserved capacity. Tools exist to correct this by
recommending a high percentile of historical usage. This project addresses a prior
question — how would you know whether such a recommendation was *good*? — which is
hard because the usage data available to evaluate a recommendation is censored by
the configuration being evaluated. It contributes a ground-truth evaluation
methodology (generative workloads, counterfactual replay against held-out demand),
a reproducible benchmark of 48,248 scored conditions, and a tested Go
implementation whose production and research paths share one recommendation
engine.

---

## What was built

**Production system** (Go 1.27, ~6,500 lines of implementation)

- Recommendation engine with pure statistical strategies and a chain of safety
  gates, separated so that experimental baselines cannot cheat
- Kubernetes discovery via client-go, including Pod → ReplicaSet → Deployment
  ownership resolution and OOMKill evidence extraction
- Prometheus integration with deliberately chosen queries (`rate()` not `irate()`,
  working set not `usage_bytes`) and documented trade-offs
- Allocation-based cost model with per-unit rates derived from instance pricing
- Versioned REST API, CLI, Prometheus instrumentation, Grafana dashboard
- Helm chart with least-privilege RBAC and render-time guardrails against
  dangerous configurations

**Research framework** (~2,000 lines)

- Generative workload simulator: ten classes with known demand, calibrated to span
  distinct failure regimes
- Counterfactual replay: recommendations scored against uncensored demand over a
  held-out horizon
- Experiment runner with matrix expansion, seeded determinism and full provenance
- Python analysis producing every figure and table in the report

**Testing** (~3,500 lines)

Unit tests cross-checked against numpy; property tests for invariants; fake
Kubernetes clientset tests; Helm chart render tests asserting RBAC properties;
end-to-end validation on a kind cluster with real load generators.

---

## What was discovered

Six findings, each measured against ground truth the engine never saw:

1. **A reliability constraint reverses the ranking of strategies.** Mean and p50
   lead on raw savings (69%) and deliver zero savings that survive the constraint.
   Evaluating savings without measuring what they cost does not merely overstate
   the benefit — it inverts the ordering.

2. **No percentile short of the maximum is universally safe.** p95 leaves 34% of
   demanded CPU work unserved on bursty workloads; on spiky workloads even p99
   leaves 8.5%. The safe percentile is determined by the workload's peak duty
   cycle, which the strategy cannot observe.

3. **The observed maximum is not an upper bound.** Sizing memory to it without a
   margin survived only 47% of conditions — the maximum of a finite sample
   estimates a distribution's tail rather than bounding it.

4. **A safety margin cannot repair a statistic that excludes the event.** Memory
   p95 plateaus at 90% survival even at a 2.0× margin, because on the bursty-memory
   class p95 is ~27% of the peak.

5. **CPU and memory fail on different workloads**, not merely to different degrees
   — which is why a unified policy must be conservative enough for the union of
   both failure sets.

6. **A short observation window is worse on every axis at once** — more aggressive,
   less reliable, and an order of magnitude more volatile. Not a trade-off.

---

## Evidence of research discipline

- **The project's own default was refuted by its own experiments.** It shipped with
  CPU p95 × 1.15; that default left 34% of demanded work unserved on bursty
  workloads. It was replaced with an evidence-derived policy and validated in a
  confirmatory run.
- **An ablation that measured nothing is reported as such.** The OOM-protection
  ablation initially returned bit-identical results because every catalog workload
  was over-provisioned and so never OOMed — the gate had nothing to act on. The
  design flaw is diagnosed in the results rather than quietly fixed.
- **The most overfitted parameter is named as such.** The burstiness threshold was
  tuned on ten synthetic classes and validated on the same ten.
- **No significance tests**, with the reasoning given: the seeds are draws from an
  author-chosen generative model, so significance would describe the simulator
  rather than workloads.
- **Related work review concludes the algorithms are not novel**, and repositions
  the contribution as methodology and study.

---

## Technical skills demonstrated

**Languages and tools** — Go (concurrency, interfaces, testing, benchmarks),
Python (pandas, numpy, scipy, matplotlib), Kubernetes (client-go, controllers,
RBAC, QoS, CFS semantics), Prometheus (PromQL, range queries, instrumentation),
Helm, Docker (multi-stage, distroless, multi-arch), kind, Make, Git.

**Engineering** — API design and versioning, structured logging, graceful
shutdown, health/readiness semantics, configuration validation, least-privilege
security, failure-mode analysis, scalability design.

**Research** — experimental design, controlled workload construction, train/test
separation, ground-truth evaluation, bootstrap and Wilson intervals, ablation and
sensitivity analysis, threats-to-validity analysis, technical writing.

---

## Honest limitations

- Findings come from synthetic workloads, not production traces. This bounds every
  claim and is the first item in the threats-to-validity document.
- The burstiness threshold was tuned and validated on the same population.
- The replay model treats the CPU request as a ceiling and memory limit as equal to
  request; both make reported risk an upper bound.
- Savings are allocation-based estimates, not spend.
- Tested at tens of workloads; scaling beyond that is a documented design, not an
  implementation.

---

## Reproducing everything

```bash
make test          # unit, property, fake-cluster and chart tests
make experiments   # 48,248 conditions, ~2 minutes
make analysis      # every figure and table
make kind-e2e      # end-to-end on a real cluster
```
