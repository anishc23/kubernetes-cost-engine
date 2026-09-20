# Threats to Validity

This document states what could make the findings in [results.md](results.md)
wrong, ordered by how likely each is to matter. It is written to be useful to
someone deciding how much weight to put on the results, which means being specific
about failure modes rather than offering general disclaimers.

---

## External validity — the largest threat

### Synthetic workloads are not production workloads

**The threat.** Every reported result comes from ten generative workload classes
designed by the author. Real services are driven by user traffic, deployment
events, retries, cache warming, garbage collection, neighbour interference and
failure cascades. None of that is represented.

**Why the study is built this way.** Ground truth is unobtainable on a real
cluster: measured usage is censored by the configuration under test (see
[methodology.md](methodology.md)). The choice was between exact ground truth on
synthetic demand and no ground truth on real demand. This study takes the first
and states the cost.

**Specific ways this could invalidate the findings.**

- Real demand is autocorrelated over long horizons — diurnal, weekly, release
  cycles — in ways the generator represents only crudely. The observation-window
  findings (RQ6) are the most exposed: a real 24-hour window may capture far more
  or far less structure than a synthetic one.
- Real bursts are correlated *across* workloads (a traffic spike hits everything).
  This study evaluates workloads independently, so it cannot see cluster-level
  effects, and correlated bursts would make the reported per-workload risk an
  underestimate of cluster-level risk.
- The classes were designed to be structurally distinct so that the strategy
  comparison would be informative. That is good experimental design and it is also
  a selection effect: real workloads may cluster in a region of the space where the
  strategies differ less.

**What would resolve it.** Replaying the same engine against public production
traces (Google Borg, Alibaba, Azure). This is the first item in
[future work](discussion.md#future-work), and until it is done, the honest
description of these findings is *"these strategies behave this way on workloads
with these structural properties"*, not *"these strategies behave this way"*.

### The burstiness threshold is tuned on this population

**The threat.** The default policy's instability threshold of 20 was chosen
because it separates the one failing class (spiky-cpu, 38–43) from the highest
safe one (bursty-cpu, 13.0) in *this* catalog. That is a gap of one data point on
each side.

This is the single most overfitted parameter in the project. With a different set
of classes, the gap could be anywhere or could not exist at all — a real workload
population might well contain workloads at burstiness 20 that are perfectly safe
at p99, and others at 10 that are not.

**How it is mitigated.** The value is a configuration field, documented as
derived-from-this-population, and disabling it is a one-line change. The procedure
by which it was chosen is stated in [results.md](results.md) so a reader can
repeat it on their own data rather than inheriting the number.

**What would resolve it.** Deriving the threshold on one workload population and
validating it on a disjoint one. The current study does neither.

---

## Internal validity

### The replay model is a model of Kubernetes, not Kubernetes

**CPU.** The replay treats the CPU request as a ceiling, which models a fully
contended node. On a node with spare capacity a container freely exceeds its
request, and the reported CPU violation rates are therefore an **upper bound**.
The direction of the bias is known and stated; its magnitude depends on cluster
utilisation, which this study does not model.

The alternative assumption was rejected for a methodological reason rather than a
conservative one: if requests never constrain, CPU under-provisioning has no
measurable consequence and RQ2 becomes unanswerable. But this means the CPU
findings apply most directly to busy clusters, which is also where cost
optimisation is most often pursued.

**Memory.** Replay sets limit = request (Guaranteed QoS). For Burstable pods
without a memory limit, a container is killed only under node pressure, so the
reported OOM rates are again an **upper bound**. Findings are exact for Guaranteed
pods.

**OOM counting.** The two-minute cooldown is a stand-in for crash-loop backoff.
Real backoff is exponential and depends on restart history, so absolute OOMKill
counts should be read as ordinal rather than as predictions of incident volume.
Comparisons between strategies, which all use the same model, are unaffected.

### Sampling resolution bounds what is observable

All traces are generated and replayed at 30-second resolution. Demand excursions
shorter than 30 seconds are not representable, so the study **cannot** observe the
failure mode of a workload whose peaks are sub-scrape-interval — which is
precisely the regime where percentile policies are most dangerous. The spiky class
approximates this with 30-second bursts, which is the finest structure the
resolution admits.

This is a real limit on the strength of Finding 2.3: the finding says no
percentile short of the maximum is safe at *this* resolution, and finer-grained
workloads would make the situation worse, not better.

### Ground truth and recommendation share a code base

The demand trace and the engine's statistics are computed by different
implementations (`internal/simulator/mathutil.go` versus `internal/stats`) on
purpose: a bug in a shared percentile estimator would affect the recommendation
and the reference it is scored against identically, the errors would cancel, and
the experiment would report success. The two are cross-checked against each other
(`TestGroundTruthPercentilesAgreeWithStatsNearestRank`) and the engine's estimator
is checked against numpy reference values.

This mitigates but does not eliminate the risk: both implementations share the
author, and a conceptual error — as opposed to a coding error — would appear in
both.

### The author selected the evaluation metrics

The feasibility constraint (zero OOMKills, ≤1% unserved CPU work) is a choice, and
a different constraint would change which configurations are feasible and
therefore which strategy "wins". The thresholds are varied in the sensitivity
analysis and the raw metrics are reported alongside the constrained ones so a
reader can apply their own constraint to the published data.

---

## Construct validity

### "Savings" measures allocation, not spend

The cost model prices reserved CPU and memory at a per-unit rate derived from an
instance price. It does not model node granularity, bin packing, autoscaler
behaviour, commitments, Spot, or the managed control plane. **Every savings figure
in this study is an upper bound on realisable spend reduction**, and the gap can be
large: freeing 200m across twenty pods saves nothing at all unless it lets the
cluster run one fewer node.

The savings figures are therefore best read *comparatively* — strategy A against
strategy B under an identical model — rather than absolutely. Comparative claims
are robust to the pricing assumption because it cancels; absolute ones are not.
See [docs/cost-model.md](../docs/cost-model.md).

### The CPU/memory price split is an assumption

Per-unit rates come from splitting an instance price 70/30 between CPU and memory.
There is no ground truth for this: providers sell machines, not separable
resources. The split shifts the *relative* weight of CPU and memory savings, which
means it could affect which resource a strategy appears to help most. It does not
affect the reliability findings, which contain no pricing at all.

### Utilisation is not performance

The study measures throttling and kills, which are proxies for what operators
actually care about: latency and error rates. A workload can be throttled without
violating its SLO, and can miss its SLO without being throttled. The mapping from
unserved CPU work to user-visible latency is not modelled, and the 1% threshold in
the feasibility constraint is a plausible operating point rather than a measured
one.

---

## Statistical conclusion validity

### Ten seeds is few

Ten realisations per condition supports interval estimation of medians and no
more. Effects smaller than the reported intervals are not resolvable, and the
1.2-percentage-point difference in Ablation C sits close to that limit — which is
why [results.md](results.md) describes it as modest rather than as a clear win.

### No significance testing, deliberately

Stated fully in [methodology.md](methodology.md). In short: with thousands of
conditions any p-value would be both underpowered per comparison and hopelessly
multiple-compared across them, and because the seeds are draws from an
author-chosen generative model, significance would be a statement about the
simulator's parameters rather than about workloads.

A reader should therefore **not** read the bootstrap intervals as hypothesis
tests. They describe variation across realisations of a synthetic process.

### Conditions are not independent observations of anything real

Each condition shares its trace with other conditions evaluating different
strategies on the same seed — which is intentional, since paired comparisons are
stronger — but it means the 49,800 records are not 49,800 independent samples. The
effective sample size for any single comparison is the number of seeds, which is
ten.

---

## What would most change the conclusions

Ranked by expected impact:

1. **Production traces.** Would test every external-validity concern at once and
   could overturn the workload-class findings entirely.
2. **A live cluster A/B deployment** with real latency measurement. Would replace
   the throttling proxy with the outcome that matters and test the replay model
   directly.
3. **A disjoint validation population for the burstiness threshold.** Would
   establish whether that parameter generalises or is an artefact.
4. **Finer sampling resolution.** Would test whether Finding 2.3 strengthens as
   peaks get shorter, as the reasoning predicts.
5. **Cluster-level cost modelling** with bin packing and autoscaling. Would convert
   allocation savings into spend estimates and is the difference between an upper
   bound and a forecast.
