# Right-Sizing Kubernetes Workloads: A Ground-Truth Evaluation of Percentile Strategies and Their Reliability Cost

**A systems study with a reproducible benchmark and an open-source implementation.**

---

## Abstract

Kubernetes reserves capacity according to declared resource requests rather than
observed usage, and operators systematically over-declare because under-declaring
fails immediately and visibly while over-declaring fails slowly and invisibly.
Automated right-sizing promises to close that gap, but evaluating it is harder than
it appears: the usage data available to such a system is *censored by the very
configuration being evaluated* — throttled CPU is never recorded as used, and a
container killed at its memory limit never records the working set it was reaching
for. Comparing a recommendation against measured usage therefore cannot distinguish
a correct recommendation from one that suppressed the evidence against it.

We address this with generative ground truth and counterfactual replay. Synthetic
workloads are generated from known demand models; the recommendation engine sees
only a censored observation of a fitting window, and its output is replayed against
uncensored demand over a *held-out* horizon. We evaluate seven strategies across ten
workload classes, four safety margins and four observation windows, for 49,800
scored conditions, using the same engine that the production controller runs.

Four findings stand out. First, applying an explicit reliability constraint
**reverses** the ranking of strategies: mean and p50 lead on raw savings (69%) and
deliver zero median feasible savings, while p99 delivers the most. Second, no
percentile short of the maximum is universally safe — whether p95 is adequate is
determined by the workload's peak duty cycle, which the strategy cannot observe.
Third, the observed maximum is an estimate rather than a bound: sizing memory to it
without a margin survived only 47% of conditions. Fourth, a safety margin cannot
repair a statistic that structurally excludes the event it must cover — p95 memory
plateaus at 90% survival even at a 2.0x margin.

These results refuted the project's own shipped default, which was replaced with an
evidence-derived policy and validated in a confirmatory run. We contribute the
evaluation methodology, a reproducible benchmark, and a tested Go implementation;
we do not contribute a novel algorithm, and the findings are bounded by the use of
synthetic workloads.

---

## 1. Introduction

A Kubernetes pod declares how much CPU and memory it wants. The scheduler reserves
that much on a node. The pod then uses whatever it uses. The difference is capacity
that is paid for and idle, and across a cluster it is typically large: published
analyses of production traces consistently report requested CPU several times
actual usage.

Operators over-declare for a rational reason. The consequences are asymmetric:
under-declaring CPU causes throttling and latency, under-declaring memory kills the
process, and both are visible within minutes and attributed to the team that
declared them. Over-declaring costs money slowly, diffusely, and is charged to
somebody else. Locally rational behaviour aggregates into systematic waste.

Tools exist to correct this — the Vertical Pod Autoscaler, Goldilocks, several
commercial platforms — and they work by computing a high percentile of historical
usage. This paper does not propose a better one. It asks a prior question:

> **How would we know whether a right-sizing recommendation was good?**

That question turns out to be harder than the recommendation itself, for a reason
developed in §3, and answering it properly changes which strategies look best.

### Contributions

1. **A methodology** for evaluating right-sizing recommendations against ground
   truth, with observation censoring modelled explicitly and scoring on a held-out
   horizon (§3).
2. **A reproducible benchmark**: ten generative workload classes with known
   structure, seeded, provenance-stamped, with the evaluation harness decoupled from
   the recommender so another engine could be compared on identical terms (§5).
3. **An empirical study** of 49,800 conditions, reporting savings alongside the
   degradation and failures that produced them (§7).
4. **An implementation**: a tested Go engine, Kubernetes controller, Prometheus
   integration, REST API, Helm chart and CLI, sharing one recommendation code path
   with the experiments (§4).
5. **Negative and self-correcting results**, including the refutation of the
   project's own default and an ablation that initially measured nothing (§8).

---

## 2. Problem definition

A workload `w` declares requests `(c_w, m_w)` for CPU and memory. Over an
observation window it exhibits demand `D_w(t)` — the resources it *would* consume
if unconstrained. A right-sizing policy `π` maps observed usage to new requests:

```
π : observed usage over window  →  (c'_w, m'_w)
```

A recommendation is **good** if it reduces cost without causing the workload to
exceed its allocation in a way that matters. The two resources differ in what
"matters" means:

- **CPU is compressible.** Demand above the request is delayed: the container is
  throttled, latency rises, the process survives. Recoverable by raising the
  request.
- **Memory is incompressible.** Demand above the limit is fatal: the kernel kills
  the process. Not recoverable, and the restart itself costs availability.

This asymmetry is the central systems fact of the problem, and it motivates
treating the two resources with separate policies rather than one parameterised
formula.

---

## 3. The evaluation problem, and why it drives the design

### 3.1 Measurement is censored by the configuration under test

Suppose we evaluate a CPU recommendation by applying it and checking measured
usage. If the request was too low, the container was throttled — so measured usage
sits *at* the request, not above it. The evaluation reports full utilisation and no
violation, for a configuration that degraded the service.

Memory is worse. A container killed at its limit never records the working set it
was reaching for; its series tops out exactly at the limit that killed it. The
observation is censored precisely at the value we need to estimate.

> **Measured usage is censored by the configuration under test, and the censoring
> is most severe exactly where the configuration is wrong.**

A corollary with practical bite: an iterative right-sizing loop that reduces a
request, observes usage under the reduced request, and reduces again is
reading its own interference as evidence of over-provisioning. §8 shows this
empirically — on a censored series, a maximum-based policy recommends the very
limit that is killing the workload (16% survival against 47% on healthy series).

### 3.2 Generative ground truth

The study separates demand from observation:

```
  generative model
        │
        ▼
   demand trace ──────────────────────────────────┐
        │                                         │
        │ censor by deployed configuration        │ (never shown to the engine)
        ▼                                         │
  observed series                                 │
        │                                         │
        ▼                                         │
  recommendation engine  ── production code ──    │
        │                                         │
        ▼                                         │
   recommendation ─────────► replay against demand ◄┘
                                    │
                                    ▼
                       throttling, OOMKills, cost
```

The engine sees the observation; it is scored against the demand, over a period it
never saw.

### 3.3 Train/test separation

Each trace splits into a fitting window and a held-out evaluation horizon. Scoring
on the fitting window would measure how well a percentile describes the data it was
computed from — arithmetic, not a result. The horizon is anchored at the end of the
trace so every window length is scored against the same future, making the window
comparison about history rather than about different futures.

### 3.4 Replay semantics

Both choices are pessimistic, and both are methodological rather than merely
cautious:

- **CPU request as ceiling**, modelling a contended node. The alternative makes CPU
  under-provisioning consequence-free in the model, which makes the CPU comparison
  uninformative. Reported CPU violation rates are an upper bound.
- **Memory limit = request** (Guaranteed QoS). Without a limit, kills depend on
  co-tenancy the model does not represent. Exact for Guaranteed pods, an upper bound
  for Burstable ones.
- **OOMKills counted per episode**, with a cooldown, so the metric counts kill–restart
  episodes rather than measuring excursion length.

---

## 4. System design

```
                        Kubernetes API (client-go)
                                  │
                                  ▼
                        Workload discovery
                   (controller ownership, requests,
                    restart and OOM evidence)
                                  │
   Prometheus ────────────────────┤
   (usage history)                │
                                  ▼
                        ┌───────────────────┐
                        │   model.Series    │ ◄── internal/simulator
                        └─────────┬─────────┘     (research mode)
                                  ▼
                        Recommendation engine
                      ┌───────────┴───────────┐
                      │  Strategy (pure stat) │
                      │  Gate chain (safety)  │
                      └───────────┬───────────┘
                    ┌─────────────┴─────────────┐
                    ▼                           ▼
              Cost estimate               Risk classification
                    └─────────────┬─────────────┘
                                  ▼
                     REST API · CLI · Prometheus metrics
```

**One engine, two modes.** The experiments invoke the same `recommender.Engine`
that the controller does, through the same entry point. Only the data source
differs, and both produce `model.Series`. This is what licenses reading the
findings as statements about the deployed system rather than about a research
prototype.

**Strategies are pure.** A `Strategy` maps a statistical summary to a target and
may not consult the current request or reliability evidence. That restriction keeps
the baselines honest: a "strategy" that peeked at the current request could
trivially never regress.

**Gates are separate, ordered, and monotone.** Safety rules — data sufficiency, OOM
protection, usage-exceeds-request, instability, floors, minimum change — are applied
after the strategy and may only hold the line or raise a target. The invariant is
`result ≥ min(proposed, current)`, asserted at runtime and in tests. It caught a
genuine design error during an experiment run: the minimum-change gate legitimately
lowers a target when cancelling a marginal *increase*, which the original,
stricter invariant had not anticipated.

**Advisory by default.** Recommendations are read-only unless `--apply` is set
explicitly; only requests are patched, never limits; and `BLOCKED` or
`INSUFFICIENT_DATA` decisions are never applied — those are exactly the cases where
the engine declined to make a claim.

---

## 5. Experimental methodology

Ten workload classes with known generative structure (§Experimental Setup). The
three CPU burst classes are calibrated to span three distinct regimes — both
percentiles see the peak, only p99 sees it, neither sees it — so that a failure can
be attributed to peak *duration* rather than peak *height*. Class properties are
asserted by tests, so a change that collapsed the regimes fails the build rather
than silently invalidating the workload-class findings.

**Metrics.** Cost (savings fraction), efficiency (utilisation), risk (violation
rate *and* throttle fraction; OOMKills and survival), accuracy against ground truth
(signed relative error, measured against p99 for CPU and against the maximum for
memory — the operating target each resource actually has), and stability (median
`|log2(r_t/r_{t-1})|` across scheduled recomputations).

**The constrained objective.** `feasible ⟺ zero OOMKills ∧ unserved CPU work ≤ 1%`.
A constraint rather than a weighted score, because a weighted score requires
choosing how many OOMKills a dollar is worth, and encoding such an exchange rate
lets a policy trade reliability at a rate no operator agreed to.

**Statistics.** Medians with 95% percentile-bootstrap intervals over 10 realisations;
Wilson intervals for proportions. No significance tests, deliberately: with
thousands of conditions any p-value is underpowered per comparison and hopelessly
multiple-compared across them, and because seeds are draws from an author-chosen
generative model, significance would describe the simulator rather than workloads.

---

## 6. Implementation

Go 1.27, roughly 6,500 lines of implementation and 3,500 of tests across
`internal/{model,stats,recommender,cost,risk,kube,promapi,simulator,experiment,api,config,metrics}`.
Deployment via Helm with least-privilege RBAC, non-root containers, a read-only
root filesystem and dropped capabilities. Observability through Prometheus metrics
on the optimizer itself, including which safety gates fired — a spike in
OOM-protection firings means workloads started failing, while a spike in
data-sufficiency firings usually means the metrics pipeline broke.

Analysis in Python (pandas, numpy, scipy, matplotlib): the production system is Go,
the scientific analysis is Python, and neither is asked to be the other.

---

## 7. Results

Full treatment with figures in [results.md](results.md). The headline findings:

| Finding | Evidence |
|---|---|
| The reliability constraint **reverses** the strategy ranking | mean/p50: 69% raw savings → **0%** feasible; p99: 57% → 18.9% |
| No percentile short of max is universally safe | bursty-cpu: p95 leaves 34% of demanded work unserved, p99 leaves 0%; spiky-cpu: **p99 leaves 8.5%**, only max succeeds |
| The observed maximum is an estimate, not a bound | memory max x1.0 survived **47%** of conditions |
| A margin cannot repair an excluding statistic | memory p95 plateaus at **90%** survival even at a 2.0x margin; max reaches 100% at 1.2x |
| CPU and memory fail on **different workloads** | CPU fails on bursty/spiky-cpu; memory fails on bursty/growing/sawtooth-memory and mixed |
| Universal safety is expensive | best universally-feasible: **31.4%** savings; best at any risk: 77.9% |
| A short window is worse on every axis | 1h vs 72h: more aggressive, 53% vs 67% feasible, 0.070 vs 0.000 volatility |

---

## 8. Ablation and self-correction

**Resource-specific policies (Ablation C)** raise the best universally-safe saving
from 30.2% to 31.4% — real, modest, and smaller than the design argument implies.
The larger effect is per-class, where the best policy varies from `mean x1.15` to
`max x1.0` across classes.

**OOM protection (Ablation A)** initially returned bit-identical results with the
gate on and off. The cause was a flaw in the experiment, not the gate: every
catalog workload is over-provisioned, so none had OOM history for the gate to act
on. **An ablation that cannot observe the component it ablates measures nothing**,
and reporting "no effect" from it would have been a false negative produced by the
design. The corrected experiment deploys workloads at 35% of their memory request,
creating the already-failing, censored population the gate exists to protect. There
it fires on 10.6% of conditions and cuts kill episodes by 12% at no cost in
savings — narrow, free, and rescuing nothing, since an under-provisioned workload
needs an increase.

**All gates removed (Ablations B, D, E)** changes pooled savings by 0.6 points and
feasibility by 1.8. But on the idle class the floor gate is the difference between
recommending 10–20m and 4.9–13.9m, the latter below the granularity at which CFS
enforces quota. Aggregate ablation metrics hide effects concentrated in a minority
of workloads.

**The project's own default was refuted.** It shipped with CPU p95 x1.15 on the
reasoning that CPU degradation is recoverable. The reasoning is sound; the default
left 34% of demanded work unserved on bursty workloads. The replacement was derived
from the data — p99 at 1.25x is safe everywhere except the spiky class, and observed
burstiness separates that class cleanly (38–43 against a maximum of 13.0 elsewhere)
— and confirmed in a dedicated run: 100% feasibility on every class and seed at
33.7% median savings, against p95's 58% savings at 80% feasibility. The cost of the
safety is stated rather than hidden, and the burstiness threshold is flagged as the
project's most overfitted parameter.

---

## 9. Threats to validity

Treated in full in [threats_to_validity.md](threats_to_validity.md). The principal
threats:

- **Synthetic workloads are not production workloads.** This bounds every finding.
  The classes were designed to be structurally distinct so the comparison would be
  informative, which is good experimental design and also a selection effect.
- **The burstiness threshold was tuned on the same population it was validated
  against** — one data point of separation on each side. The most overfitted
  parameter in the project.
- **Replay is a model of Kubernetes, not Kubernetes.** CPU-as-ceiling and
  limit=request both make reported risk an upper bound, in a known direction of
  known sign but unknown magnitude.
- **30-second resolution** means sub-scrape excursions are unrepresentable — the
  regime where percentile policies are most dangerous.
- **"Savings" measures allocation, not spend.** Comparative claims are robust to the
  pricing assumption because it cancels; absolute ones are not.

---

## 10. Related work

The algorithms here are not new. Percentile-based right-sizing with asymmetric
CPU/memory handling is the Vertical Pod Autoscaler's design; Goldilocks surfaces
it; Kubecost and OpenCost solve the adjacent and substantially harder problem of
allocating real cloud spend. **No claim of superiority over any of them is made.**

What appears not to exist in this specific form is a ground-truth evaluation of
these strategies with observation censoring modelled explicitly and reliability
scored rather than assumed. That is a claim about a *methodology and a study*, which
is a much smaller claim than a novel algorithm — and it is the claim the work
supports. See [related_work.md](related_work.md).

---

## 11. Conclusion

The question this study set out to answer was not how to right-size Kubernetes
workloads, but how to tell whether a right-sizing recommendation was any good. That
question is harder than it looks, because the data available to answer it is
censored by the decision being evaluated.

Taking the censoring seriously changes the answers. Under a reliability constraint
the ranking of strategies reverses. No percentile short of the maximum is
universally safe, and which one suffices is determined by a workload property the
strategy cannot see. The observed maximum is not a bound, and a safety margin
cannot rescue a statistic that excludes the event it must cover.

Most usefully for the credibility of the rest: these experiments refuted the
project's own default policy, and an ablation was found to be measuring nothing at
all. Both are reported. A study whose experiments can only confirm its design is not
measuring the design.

---

## 12. Future work

Ordered by expected value: replay against production traces; a live A/B deployment
with latency measurement; cluster-level cost modelling with bin packing; adaptive
policy selection evaluated against the static baselines established here; joint
request/limit optimisation; uncertainty-aware recommendations; carbon-aware
multi-objective optimisation.

Deliberately excluded: a deep-learning recommender. Nothing in these results
suggests the bottleneck is model capacity. The failures observed come from
statistics that structurally exclude the events they must cover, and from
measurement censored by the configuration under test. Neither is fixed by a more
expressive function approximator, and both are made harder to diagnose by one.

---

## Reproducing this report

```bash
make test          # unit, integration and property tests
make experiments   # 49,800 conditions, ~2 minutes
make analysis      # every figure and table in this report
make kind-e2e      # end-to-end validation on a real cluster
```

Every figure and table is generated by `analysis/scripts/figures.py` from the
result files in `experiments/results/`. Nothing in this report is transcribed by
hand.

## Document map

| Document | Contents |
|---|---|
| [research_questions.md](research_questions.md) | RQs and falsifiable predictions |
| [methodology.md](methodology.md) | Censoring, replay, metrics, statistics |
| [experimental_setup.md](experimental_setup.md) | Workload catalog, configs, reproduction |
| [results.md](results.md) | Full results with figures |
| [discussion.md](discussion.md) | Interpretation and implications |
| [limitations.md](limitations.md) | What the system does not do |
| [threats_to_validity.md](threats_to_validity.md) | What could make the findings wrong |
| [related_work.md](related_work.md) | What exists and what is different |
