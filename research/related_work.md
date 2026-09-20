# Related Work

The purpose of this document is to answer one question honestly: **given what
already exists, what is actually different about this project?**

The short answer: the *algorithms* are not new. Percentile-based right-sizing is
implemented in the Kubernetes Vertical Pod Autoscaler, surfaced by Goldilocks, and
sold by several commercial platforms. What this project contributes is an
**evaluation methodology and an empirical study**, not a recommender.

---

## Kubernetes Vertical Pod Autoscaler (VPA)

The reference implementation in this space, part of the Kubernetes autoscaler
project.

**What it does.** Maintains a decaying histogram of CPU and memory usage per
container and recommends a target at a high percentile of that histogram, with
memory handled more conservatively than CPU. Can apply recommendations by evicting
and recreating pods. Exposes a bounded recommendation (lower bound, target, upper
bound, uncapped target).

**Relationship to this project.** The core insight — percentile-based targets with
CPU and memory treated differently — is VPA's, not this project's. This project's
engine is a simpler, explicitly-parameterised reimplementation of the same family
of ideas, built to be *comparable across configurations* rather than to be the
best single recommender.

**What this project adds.** VPA does not publish a controlled evaluation of how its
percentile choices perform against ground truth on workloads of known structure,
and its histogram decay makes the effective observation window implicit rather
than a parameter one can sweep. The strategy/margin/window comparison here is the
kind of study one would want before choosing VPA's parameters.

**What VPA does better.** Continuous operation, in-place resize support on newer
Kubernetes versions, eviction-aware application, production maturity across many
clusters, and integration with the autoscaler ecosystem. This project does not
attempt any of that.

---

## Goldilocks (Fairwinds)

**What it does.** Runs VPA in recommendation-only mode across namespaces and
presents the results in a dashboard, making VPA's recommendations visible without
enabling its mutation.

**Relationship to this project.** Goldilocks and this project's production mode
occupy the same niche — surfacing right-sizing advice without applying it. The
overlap is substantial and should be acknowledged rather than minimised.

**What this project adds.** A cost model, explicit risk classification, per-decision
evidence and reasoning, safety gates that can veto a reduction and say why, and
the experimental framework. Goldilocks presents VPA's numbers; this project
presents its own numbers *and* the evidence behind them.

**What Goldilocks does better.** It builds on VPA rather than reimplementing it,
which means it inherits VPA's maturity. It is simpler to deploy and has real
production usage.

---

## Kubecost / OpenCost

**What they do.** Cluster cost allocation and showback: attributing real cloud
spend to namespaces, workloads, teams and labels, integrating with provider
billing APIs including Reserved Instances and Spot, and offering right-sizing as
one feature among many.

**Relationship to this project — different problems.** Kubecost's central problem
is *allocation*: given a real bill, who spent it? This project's central problem is
*right-sizing*: given usage history, what request is both cheaper and safe? The
cost model here exists to make right-sizing decisions comparable, and it is
deliberately an allocation-based estimate rather than a billing integration.

**What Kubecost does much better.** Actual cloud billing integration, amortisation
of commitments, Spot pricing, multi-cluster aggregation, network and storage costs,
showback and chargeback workflows. Its cost numbers are grounded in invoices; this
project's are explicitly an upper bound derived from a configured rate.

**This project makes no claim to be better than Kubecost**, and the comparison is
not one of degree. A team wanting to know what their cluster costs should use
Kubecost or OpenCost. This project answers a narrower question with more
methodological care about the answer's reliability.

---

## Academic and industrial background

The problem this project sits in has a substantial literature, summarised here by
the ideas that bear on the design rather than as an exhaustive survey.

**Cluster trace characterisation.** Published analyses of large production traces
(Google's Borg traces, Alibaba's cluster traces, Azure's VM traces) consistently
report a large gap between requested and used resources, with requested CPU often
several times actual usage. These motivate the problem and supply the workload
structures the generator imitates; they are also the obvious next step for this
project, since replaying the same engine against them would address its principal
external-validity threat.

**Autoscaling and resource management.** A long line of work on horizontal and
vertical scaling, predictive versus reactive policies, and SLO-aware resource
allocation. The recurring lesson relevant here is that percentile targets are a
weak predictor for workloads with heavy-tailed demand — consistent with the
finding in RQ2 that no percentile short of the maximum was universally safe.

**Scheduling and bin packing.** The literature on cluster scheduling establishes
that per-workload right-sizing is only half the problem: freed capacity becomes
savings only through packing. This is the direct source of the cost model's
principal limitation, and the reason this project reports an upper bound rather
than a forecast.

**Censoring in systems measurement.** The observation that a system's telemetry is
shaped by the constraints under which it was collected is not novel — it appears in
queueing measurement, in capacity planning and in performance evaluation
generally. Its specific consequence for Kubernetes right-sizing, that the
working-set series of an OOMKilled container is censored at exactly the value one
needs to estimate, is the observation this project's methodology is built around.

---

## What is genuinely different here

Stated precisely, and no more strongly than the evidence supports:

1. **Ground-truth evaluation via counterfactual replay.** The engine is scored
   against demand it never observed, over a period it did not fit to, with the
   censoring of observation modelled explicitly. Production tools cannot do this,
   because on a real cluster the ground truth does not exist. This is the
   contribution the rest depends on.

2. **Reliability as a first-class result rather than an assumption.** Savings are
   reported alongside the degradation and failures that producing them caused, and
   under an explicit constraint. The finding that this *reverses* the strategy
   ranking (Finding 2.1) is only visible because both were measured.

3. **A reproducible benchmark others can reuse.** Version-controlled workload
   classes with known generative structure, seeded and provenance-stamped, with the
   evaluation harness separate from the recommender. Another right-sizing engine
   could be dropped into the same harness and compared on identical terms.

4. **Negative and self-correcting results reported.** The project's own default was
   refuted by its own experiments and replaced. An ablation that measured nothing
   is reported as such, with the design flaw diagnosed, rather than quietly fixed
   or omitted.

## What is not claimed

- **No novel algorithm.** Every strategy evaluated is a standard statistic.
- **No superiority over VPA, Goldilocks or Kubecost.** Different scope, far less
  production validation.
- **No production validation.** Results come from synthetic workloads; the
  end-to-end test validates the collection path, not the findings.
- **No claim that the findings generalise to real workloads.** That is the open
  question, and [threats_to_validity.md](threats_to_validity.md) says so first.

If the related-work review had shown that this evaluation already existed, the
correct response would have been to reposition the project as a replication.
It appears not to, in this specific form — but the claim being made is about a
*methodology and a study*, which is a much smaller claim than a novel algorithm,
and it is the claim the work supports.
