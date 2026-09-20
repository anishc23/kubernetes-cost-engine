# Research Summary

*A one-page statement of the research contribution, suitable for a research
group application or as the opening of a longer conversation.*

---

## The question

Kubernetes reserves cluster capacity according to *declared* resource requests
rather than observed usage. Operators systematically over-declare, for a rational
reason: under-declaring fails immediately and visibly (throttling, or an OOMKill),
while over-declaring fails slowly, diffusely, and on someone else's budget.
Automated right-sizing is the obvious correction, and several mature
implementations exist.

This project addresses a prior question that the existing tools do not answer:

> **How would you know whether a right-sizing recommendation was good?**

## Why that question is hard

The usage data available to evaluate a recommendation is **censored by the
configuration being evaluated**.

If a CPU request is too low, the container is throttled, so measured usage sits
*at* the request rather than above it — the evaluation reports full utilisation
and no violation for a configuration that degraded the service. If a memory limit
is too low, the container is killed, and its working-set series tops out at
exactly the limit that killed it.

The measurement is therefore biased, not merely noisy, and it is biased most
severely in precisely the cases where the configuration is wrong. Comparing a
recommendation against measured usage cannot distinguish "the recommendation was
correct" from "the recommendation suppressed the evidence that would have
falsified it".

A corollary with practical consequences: an iterative right-sizing loop that
reduces a request, observes usage under the reduced request, and reduces again is
reading its own interference as evidence of over-provisioning.

## Approach

Separate demand from observation using a generative model:

- **Demand** is what the workload wants, independent of any configuration.
- **Observation** is demand after the deployed configuration has interfered with
  it — which is what a monitoring system records.

The recommendation engine sees only the observation, over a *fitting window*. Its
output is then replayed against uncensored demand over a **held-out evaluation
horizon** it never saw. Scoring on the fitting window would measure how well a
percentile describes the data it was computed from, which is arithmetic rather
than a result.

Ten workload classes were designed with distinct generative structure, calibrated
so that their peak duty cycles span three regimes: peaks visible to both p95 and
p99, visible only to p99, and visible to neither. That calibration is asserted by
tests, so a change that collapsed the regimes fails the build rather than silently
invalidating the workload-class findings.

## Principal findings

Across 49,800 scored conditions:

1. **An explicit reliability constraint reverses the ranking of strategies.**
   Mean- and median-based policies lead on raw savings (69%) and deliver *zero*
   savings that survive the constraint. Evaluating cost reduction without measuring
   what it cost does not merely overstate the benefit; it inverts the ordering.

2. **No percentile short of the maximum is universally safe.** The safe percentile
   is determined by the workload's peak duty cycle — a property of the workload
   that the strategy cannot observe. Any fixed percentile recommendation is
   therefore a bet on the workload population it will encounter.

3. **The observed maximum is an estimate, not a bound.** Sizing memory to the
   maximum of the fitting window survived only 47% of held-out horizons. Extreme
   value statistics is the right framing, and it is largely absent from production
   right-sizing tooling.

4. **A safety margin cannot repair a statistic that structurally excludes the
   event it must cover.** Memory p95 plateaus at 90% survival even at a 2.0×
   margin, because on one workload class p95 of the working set is ~27% of its
   peak. Increasing a multiplier is the usual response to an occasionally-unsafe
   policy; it works only when the statistic is close to the event.

5. **CPU and memory fail on different workloads**, not merely to different
   degrees, which constrains any unified policy to the union of both failure sets.

## Methodological commitments

- **The experiments exercise the deployed engine.** Production and research modes
  share one recommendation code path; only the data source differs. Findings are
  therefore statements about the shipped system rather than about a prototype.
- **The evaluation could produce unwelcome results, and did.** The project's own
  default policy was refuted by its own experiments and replaced with an
  evidence-derived one. An ablation that turned out to be incapable of observing
  the component it ablated is reported as such, with the design flaw diagnosed.
- **No significance testing**, with the reasoning stated: the seeds are draws from
  an author-chosen generative model, so a significant difference would describe the
  simulator rather than workloads. Effect sizes with bootstrap intervals answer the
  question that can actually be answered.
- **The most overfitted parameter is named.** A burstiness threshold in the
  revised default was tuned on ten synthetic classes and validated on the same ten.

## What is claimed, and what is not

**Claimed:** an evaluation methodology, a reproducible benchmark, and an empirical
study. The algorithms evaluated are standard statistics; percentile-based
right-sizing with asymmetric CPU/memory handling is the Vertical Pod Autoscaler's
design, not this project's.

**Not claimed:** a novel algorithm, superiority over existing tools, production
validation, or that the findings generalise to real workloads. The last is the
principal limitation: the honest description is *"these strategies behave this way
on workloads with these structural properties"*.

## Next step

Replaying the same engine against public production traces (Google Borg, Alibaba,
Azure) would test every external-validity concern at once and could overturn the
workload-class findings. The engine already consumes a generic time-series type,
so this is primarily a data-ingestion exercise — and it is the obvious extension
of the work.
