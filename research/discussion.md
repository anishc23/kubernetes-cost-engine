# Discussion

## What the results mean

### Savings figures without a reliability constraint can be actively misleading

The strongest methodological result here is Finding 2.1: applying the reliability
constraint does not merely reduce the reported savings, it **reverses the ranking
of strategies**. Mean and p50 lead on raw savings at 69% and produce zero median
feasible savings. p99, which looks unremarkable at 57%, is the only strategy with
substantial savings that survive the constraint.

The mechanism is simple once stated. A strategy that under-provisions saves more,
by definition, and the saving is real — it is the consequence that is unreported.
Any evaluation that measures the numerator and not the denominator will
systematically prefer the strategies that fail most.

This bears directly on how cost-optimisation tooling is usually presented. A
dashboard reporting "$40k/month identified savings" is reporting the numerator. The
question a reader should ask is what the recommendation would have done to the
workloads, and that question is answerable only with something like the ground
truth this study constructs.

### The safe percentile is a property of the workload, not of the statistic

Finding 2.3 is the result with the most direct practical consequence. On bursty-cpu
(bursts 3.3% of the duty cycle), p95 leaves 34% of demanded work unserved and p99
leaves none. On spiky-cpu (0.2%), even p99 leaves 8.5% unserved and only the
maximum succeeds.

The pattern is not subtle: a percentile policy is safe exactly when the workload's
peak duty cycle exceeds the percentile's tail mass. That is a property of the
workload, and the strategy cannot observe it. Any fixed percentile recommendation —
including "use p95", which is the field's default advice — is therefore a bet on the
workload population it will meet.

The practical implication is not "use p99 everywhere" but "a right-sizing system
should measure the property that determines whether its policy is safe, and decline
when it is not". That is what the revised default does, using burstiness as the
observable proxy.

### The maximum is an estimate, not a bound

Finding 3.1 contradicts a common intuition sharply enough to be worth restating:
sizing memory to the maximum observed in the fitting window, with no margin,
survived **47%** of conditions. "Just use the maximum and you are safe" is wrong
more than half the time.

The reason is that the maximum of a finite sample estimates the upper tail of a
distribution; it does not bound it. The next window contains draws the last one did
not. Extreme-value statistics is the right framing for this, and it is a framing
absent from most right-sizing tooling, which treats the observed maximum as though
it were a hard ceiling.

### A safety margin cannot repair a statistic that excludes the event

Finding 3.2 sharpens this. p95 memory plateaus at 90% survival and stays there at a
2.0x margin, while max reaches 100% at 1.2x. On the bursty-memory class, p95 of the
working set is roughly 27% of its peak, so doubling p95 still lands below the peak.

This matters because safety margins are the usual response to a policy that is
occasionally unsafe: if it fails, increase the multiplier. That response works when
the statistic is close to the event and fails entirely when it is not, and the
distinction is not visible from the failure rate alone — only from knowing where the
statistic sits relative to the demand it must cover.

### CPU and memory fail on different workloads

The design claim that motivated the engine's structure was that CPU and memory need
different policies because their failure modes differ. The results support it, but
the informative form is not the aggregate: separating the policies raises the best
universally-safe saving by 1.2 percentage points, which is modest.

The substantive result is in Finding 4.1 and the heatmap: the two resources fail on
**different classes**. CPU percentile policies fail on bursty-cpu and spiky-cpu.
Memory percentile policies fail on bursty-memory, growing-memory, sawtooth-memory
and mixed — a nearly disjoint set. A unified policy must therefore be conservative
enough for the union of both failure sets, which is why the unified ablation's
universally-feasible space is smaller.

### A short observation window is not a trade-off

Finding 6.2 is a clean negative result for a plausible intuition. One might expect a
short window to buy freshness at the cost of accuracy. It does not: at one hour, p95
is simultaneously more aggressive (73% savings against 67%), less reliable (53%
feasible against 67%), and an order of magnitude more volatile.

The extra saving is not a saving. It is the absence of evidence — the peaks have not
happened yet — being read as evidence of absence. This is the same error the OOM
protection gate exists to prevent, appearing at a different level of the system.

### Some components matter narrowly, and that is worth saying

Ablation A found that OOM protection fires on 10.6% of at-risk conditions, cuts kill
episodes by 12% in the worst cell, and costs nothing — but does not improve survival,
because an already-failing workload needs an increase rather than protection from a
decrease. Ablation A.4 found that removing *every* gate changes pooled savings by 0.6
points.

The temptation is to report the gates as essential safety machinery. The accurate
description is narrower: they prevent specific harmful actions in specific
circumstances, and their aggregate effect is small because those circumstances are a
minority of workloads. That is still worth having — a component that prevents a rare
catastrophic action at no cost is a good trade — but it is a different claim from
"the safety machinery drives the results".

## What this changed about the system

The clearest evidence that the experiments were capable of producing unwelcome
results is that they did. The project shipped with CPU p95 x1.15 on the reasoning
that CPU degradation is recoverable and therefore tolerates a lower percentile. The
reasoning is sound; the default left 34% of demanded work unserved on bursty
workloads.

The replacement was derived from the data rather than re-guessed: p99 at 1.25x is
safe on every class except spiky-cpu; observed burstiness separates that class
cleanly (38–43 against a maximum of 13.0 elsewhere); an instability gate at 20
should therefore withhold the reduction exactly where p99 fails. The confirmatory
run supports that prediction — 100% feasibility on every class and seed at 33.7%
median savings.

Two things about that revision deserve scepticism, and are stated in
[results.md](results.md) and [threats_to_validity.md](threats_to_validity.md)
rather than left implicit. The threshold was tuned on the same ten classes it was
validated against, which is the most overfitted parameter in the project. And the
safety is bought: p95 x1.25 saves 58% against 34%, and an operator who knows their
workloads are not spiky is right to prefer it.

## Implications for practitioners

1. **Ask what a savings figure cost.** A right-sizing recommendation without an
   accompanying reliability estimate is half a result.
2. **Do not treat p95 as a default.** Whether it is safe depends on your workloads'
   peak duty cycle. Measure burstiness before trusting it.
3. **Do not treat the observed maximum as a ceiling for memory.** It survived less
   than half the time here without a margin.
4. **Prefer a longer observation window.** In this study nothing favoured a short
   one; it was worse on savings validity, reliability and stability together.
5. **Treat memory reductions as categorically riskier than CPU reductions.** The
   failure is a kill, the evidence is censored, and the two together mean the data
   is least trustworthy exactly when the decision matters most.

## Implications for tool builders

1. **Evaluate against something the recommender did not see.** Comparing a
   recommendation against measured usage is close to circular, because measurement
   is censored by the configuration under test.
2. **Publish the constraint, not just the objective.** Reporting savings under an
   explicit reliability constraint would make tools comparable in a way that
   headline savings figures do not.
3. **Instrument the declines.** Recommendations that were withheld, and why, are
   more informative about a system's judgement than the ones it emitted.

## Future work

Ordered by expected value, which is roughly the inverse of how easy each is.

1. **Replay against production traces** (Google Borg, Alibaba, Azure). Directly
   addresses the study's principal threat and could overturn the workload-class
   findings. The engine already consumes `model.Series`, so this is primarily a
   data-ingestion exercise.
2. **A live A/B deployment with latency measurement.** Would replace the throttling
   proxy with the outcome that matters, and test the replay model against reality.
3. **Cluster-level cost modelling** with bin packing and autoscaler simulation.
   Converts allocation savings into spend estimates, which is the difference between
   an upper bound and a forecast.
4. **Adaptive policy selection.** The per-class results show the best policy varies
   sharply by workload. A policy that selects its percentile from measured
   burstiness is the obvious extension — and must be evaluated against the static
   baselines here, not asserted to be better. The infrastructure for that comparison
   already exists.
5. **Joint request/limit optimisation.** Currently out of scope, and the more
   valuable half of the problem for memory, where the limit determines the kill
   threshold.
6. **Uncertainty-aware recommendations.** Reporting an interval rather than a point,
   with the width driven by data sufficiency and workload stability. The engine
   already computes the inputs.
7. **Carbon-aware and multi-objective optimisation.** Reserved capacity has an
   energy cost as well as a financial one, and the two do not always agree.

Deliberately **not** on this list: a deep-learning recommender. Nothing in these
results suggests the bottleneck is model capacity. The failures observed here come
from statistics that structurally exclude the events they need to cover, and from
measurement that is censored by the configuration under test. Neither is fixed by a
more expressive function approximator, and both are made harder to diagnose by one.
