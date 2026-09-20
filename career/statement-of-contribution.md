# Statement of Contribution

*A precise account of what was built, what was discovered, and what was borrowed —
written so that each claim can be checked against the repository.*

---

## Independent work

All code, experimental design, analysis and documentation in this repository is my
own, written from scratch. No portion is derived from another right-sizing
implementation.

## What I designed

**The evaluation methodology.** The central contribution. Recognising that
observed usage is censored by the configuration under test, and constructing an
evaluation that separates demand from observation, censors the observation
deliberately, and scores recommendations against demand over a held-out horizon.

**The workload taxonomy.** Ten generative classes calibrated so their peak duty
cycles span three distinct regimes relative to p95 and p99. The calibration was
iterative: an initial version had burst intervals that placed every class's peaks
inside p99's reach, which would have made the strategy comparison uninformative. I
diagnosed this by measuring the p95/max and p99/max ratios per class and adjusted
the duty cycles, then asserted the resulting properties in tests.

**The constrained objective.** Choosing a feasibility constraint over a weighted
score, because a weighted score requires an exchange rate between dollars and
OOMKills that nobody can defend, and because encoding one lets a policy trade
reliability at a rate no operator agreed to.

**The safety gate architecture.** Separating pure statistical strategies from
evidence-consuming gates, with an ordered chain and a monotonicity invariant. The
separation is what keeps the experimental baselines honest.

## What I discovered

Six findings, listed in `career/project-summary.md` and documented with evidence in
`research/results.md`. The three I consider most valuable:

1. That a reliability constraint *reverses* the strategy ranking — which means
   savings-only evaluation is not conservative, it is wrong.
2. That the observed maximum is not an upper bound (47% survival), contradicting a
   widely held intuition.
3. That a safety margin cannot rescue a statistic that structurally excludes the
   event — a distinction invisible from failure rates alone.

## What I got wrong, and corrected

These are included deliberately, because a project whose experiments only ever
confirm its design is not measuring the design.

**The default policy.** I shipped CPU p95 × 1.15 reasoning that CPU degradation is
recoverable and therefore tolerates a lower percentile. The reasoning is sound and
the default was wrong: it left 34% of demanded CPU work unserved on bursty
workloads. I derived a replacement from the data and validated it in a separate
confirmatory run.

**An ablation that measured nothing.** The OOM-protection ablation returned
bit-identical results with the gate enabled and disabled. My first hypothesis was a
wiring bug. It was an experimental design flaw: every catalog workload was
over-provisioned, so none ever OOMed, so the gate had no evidence to act on. I
added a configuration that deploys workloads below their peak demand — creating the
already-failing, censored population the gate exists to protect — and report both
versions.

**A safety invariant that was too strong.** I asserted that no safety gate may
lower a recommendation. It panicked mid-experiment on the minimum-change gate
cancelling a 9% proposed *increase* — which is correct behaviour. The invariant
should have been `result ≥ min(proposed, current)`. My test had passed because it
lacked the near-current increase cases; I fixed both.

**A CLI that silently ignored flags.** Go's flag package stops parsing at the
first non-flag argument, so `koctl recommend --namespace prod` dropped every flag
without warning. Found by using the tool against a real cluster, not by any test.

## What I borrowed

**Ideas, with attribution** (see `research/related_work.md`):

- Percentile-based right-sizing with asymmetric CPU/memory handling is the
  Kubernetes Vertical Pod Autoscaler's design.
- The recommendation-only operating model is Goldilocks' contribution.
- The observation that per-workload right-sizing is only half the problem, and
  that bin packing is the other half, comes from the cluster scheduling literature
  and is why the cost model reports an upper bound.

**Libraries:** client-go, the Prometheus Go client, sigs.k8s.io/yaml; pandas,
numpy, scipy, matplotlib for analysis.

## Scope and honesty

The strongest claim the evidence supports:

> On controlled synthetic workloads with known ground truth, these right-sizing
> strategies behave in these measurable ways, and the differences between them are
> large enough to matter operationally.

Not supported, and stated as such throughout: production validation, superiority
over existing tools, a novel algorithm, or generalisation to real workloads.

## Tooling disclosure

This project was developed with AI assistance for implementation and drafting. The
research questions, evaluation methodology, workload design, interpretation of
results and the decisions to report negative findings are mine, and I can defend
each design choice and each finding on its technical merits.
