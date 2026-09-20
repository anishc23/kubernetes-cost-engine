# Interview Guide

Preparation notes for discussing this project technically. The questions are the
ones a competent interviewer would actually ask, and the answers are the honest
ones — including where the project is weak.

**The single most important thing:** the interesting part of this project is *not*
that it right-sizes Kubernetes workloads. That is a solved problem with several
mature implementations. The interesting part is the evaluation methodology, and
the fact that applying it changed the answer.

---

## The 60-second version

> Kubernetes reserves capacity by declared request, not by usage, so clusters
> carry a lot of idle reserved capacity. Tools exist to fix that by recommending a
> high percentile of historical usage.
>
> I got interested in a prior question: how would you know whether such a
> recommendation was good? It turns out you mostly can't, because the usage data
> is censored by the configuration you're evaluating — throttled CPU is never
> recorded as used, and a container killed at its memory limit never records the
> working set it was reaching for.
>
> So I built a generative workload simulator with known ground truth. The engine
> only ever sees a censored observation of a training window, and I score its
> recommendation by replaying it against real demand over a held-out period.
>
> The headline result: adding a reliability constraint *reverses* the ranking of
> strategies. Mean and p50 look best on raw savings at 69%, and deliver zero
> savings that survive the constraint. And my own shipped default — p95 for CPU —
> turned out to leave 34% of demanded CPU work unserved on bursty workloads, so I
> replaced it with something the data actually supported.

---

## Kubernetes

**Why are requests different from limits?**

The request is what the scheduler reserves — it determines placement and becomes
`cpu.shares` for CPU. The limit is a hard cap enforced by the kernel: a CFS quota
for CPU, and for memory the threshold at which the OOM killer fires. A pod with
request < limit is Burstable: it's guaranteed its request and may use more if the
node has spare capacity.

**What happens when a container exceeds its CPU request?**

Nothing, if the node has spare capacity — the request is a weight, not a cap.
Under contention, the CFS scheduler allocates proportionally to shares, so it gets
roughly its request's worth and is delayed beyond that. Only a CPU *limit* imposes
a hard quota.

*This matters for my evaluation:* I model the request as a ceiling, which assumes
a contended node. That's a pessimistic assumption and I document it as one. The
reason isn't caution — it's that if containers can always burst above their
request, CPU under-provisioning has no measurable consequence in the model, every
strategy scores identically on CPU risk, and the comparison becomes uninformative.

**What happens when memory is exceeded?**

The kernel OOM killer terminates the process. There's no throttling equivalent —
you can't give a process less memory than it allocates. The container restarts,
in-flight requests are lost, and with a crash loop you get exponential backoff.

**So why can't you treat memory like CPU?**

Three reasons, and the third is the one people miss:

1. The failure is fatal rather than degraded.
2. It's not recoverable by the workload itself.
3. **The measurement is censored.** An OOMKilled container's working-set series
   tops out exactly at the limit that killed it. The data says "you're using less
   than you reserved" precisely *because* the process keeps dying before it can use
   more. That's the argument for a separate memory policy and for the OOM
   protection gate.

**How do you map a pod back to its workload?**

Follow `ownerReferences`. The awkward case is Deployments: the chain is
Pod → ReplicaSet → Deployment, and the pod can't see the Deployment. I recover the
name by stripping the ReplicaSet's pod-template-hash suffix — a string operation
on a name, which is fragile, so it's confined to one function. If it fails, the
pod contributes no evidence rather than evidence attributed to the wrong workload.

**Why recommend per workload rather than per pod?**

The request lives in the pod template. A per-pod recommendation is advice about an
object that will be replaced on the next rollout.

---

## Prometheus

**Why `rate()` and not `irate()`?**

`irate()` uses only the last two samples in each step, so over a multi-day window
it amplifies scrape jitter into apparent spikes. For "how much CPU does this
container consume", `rate()` is the right estimator.

The cost, which I document: `rate()` smooths, so bursts shorter than the rate
window are attenuated. That biases percentile policies toward under-provisioning
for bursty workloads — in the *same direction* as one of my findings, which means
it could be a partial cause of that finding on a real cluster.

**Why working set and not `container_memory_usage_bytes`?**

Working set excludes reclaimable page cache, and it's what the kernel compares
against the limit when deciding to OOM kill. Sizing against `usage_bytes` would
systematically over-provision every container that reads files.

**What are the limitations of Prometheus sampling here?**

Resolution bounds what's observable. At a 1-minute step with a 4-minute rate
window, sub-minute bursts may not appear in any percentile. That's a real limit on
my strongest finding: it says no percentile short of the maximum is safe *at this
resolution*, and finer-grained workloads would make it worse, not better.

**Why one query per workload rather than per pod?**

The workload shares one request, so the engine needs the aggregate behaviour of
the replica set. I `sum()` with an anchored pod regex. The anchoring matters — an
unanchored pattern for a workload named `api` would also match `api-gateway`'s
pods, and you'd compute one workload's recommendation from another's usage.

---

## Systems and statistics

**Why p95? Why not p99 or max?**

This is the question the project exists to answer, and my initial answer was
wrong.

I shipped p95 for CPU on the reasoning that CPU degradation is recoverable so it
tolerates a lower percentile. Measured against held-out demand, p95 left **34% of
demanded CPU work unserved** on bursty workloads.

The mechanism: a percentile policy is safe exactly when the workload's peak duty
cycle exceeds the percentile's tail mass. My bursty class has bursts 3.3% of the
time, so p95 sits below them and p99 above. My spiky class has 0.2%, so **even p99
fails** and only the maximum works.

The safe percentile is a property of the *workload*, not of the strategy — and the
strategy can't observe it. So any fixed recommendation, including "use p95", is a
bet on the workload population you'll meet.

**So what did you change it to?**

p99 × 1.25, plus an instability gate that withholds CPU reductions above 20×
burstiness. I derived that from the data: p99 is safe on every class except the
spiky one, and observed burstiness separates that class cleanly — 38-43 versus a
maximum of 13 everywhere else. A confirmatory run gave 100% feasibility across
every class and seed at 34% median savings.

**What's wrong with that fix?**

It's the most overfitted parameter in the project. I chose the threshold on ten
synthetic classes and validated it on the same ten, with one data point of
separation on each side. On real workloads the gap might not exist. I say so in
the threats-to-validity document, and the parameter is configurable.

**Why does memory use `max` when you just argued against fixed rules?**

Two measured findings. First, **the observed maximum isn't an upper bound** —
sizing memory to it with no margin survived only 47% of conditions, because the
max of a finite sample estimates a distribution's tail rather than bounding it.
Hence the 1.25× margin.

Second, and more interesting: **a safety margin can't repair a statistic that
excludes the event.** Memory p95 plateaus at 90% survival and stays there at a
2.0× margin, because on my bursty-memory class p95 is about 27% of the peak.
Doubling it still lands below the peak. That's a structural failure, not a
tuning problem.

**How does window length affect things?**

Not the way I expected. I assumed short windows trade accuracy for freshness. They
don't — at one hour, p95 was *simultaneously* more aggressive (73% savings vs
67%), less reliable (53% feasible vs 67%), and an order of magnitude more
volatile. Worse on every axis.

The extra saving isn't a saving. It's absence of evidence — the peaks haven't
happened yet — being read as evidence of absence.

There's also an interaction I didn't anticipate: savings under a `max` policy
*fall* as the window grows (55% → 33%), because a longer history contains rarer
and taller peaks. Window length isn't a neutral parameter applied to a fixed
strategy; it changes what the strategy means.

**Why medians and bootstrap intervals rather than means and t-tests?**

The distributions aren't symmetric — savings are bounded above by 1, throttle
fractions bounded below by 0 with a long right tail, OOM counts zero-inflated. A
normal-theory interval assumes away exactly that structure and would put interval
bounds below zero for quantities that can't be negative.

**Why no significance tests at all?**

Two reasons. Practically: thousands of conditions with ten seeds each means any
p-value is underpowered per comparison and hopelessly multiple-compared across
them. More fundamentally: the seeds are draws from a generative model *I chose*, so
a significant difference would be a statement about my simulator's parameters, not
about workloads. Effect sizes with intervals answer the question that can actually
be answered.

---

## Distributed systems and operations

**What happens if Prometheus goes down?**

The previous snapshot keeps being served, and liveness keeps passing —
deliberately. A liveness probe that failed on a dependency outage would restart a
healthy optimizer and turn an outage into a crash loop, making recovery slower.
Readiness *does* fail, so the Service stops routing. Staleness is exposed as a
metric, and that's what you alert on.

**Why serve a stale snapshot rather than nothing?**

A recommendation from an hour ago with a visible timestamp is more useful than an
error. The failure mode I'm guarding against is a *confidently wrong* answer, not
an old one.

**What if the Kubernetes API fails?**

The cycle aborts and the previous snapshot is served. But a single *namespace*
failing doesn't abort the pass — RBAC commonly grants access to a subset, so
partial results plus a logged error beat nothing.

**How do you prevent stale recommendations being acted on?**

`optimizer_last_analysis_timestamp_seconds` is exported and prominent on the
dashboard; responses are `Cache-Control: no-store`; readiness requires a completed
cycle so an empty result set is never served as "nothing to optimise".

**How would this scale to 100,000 workloads?**

It wouldn't, as built, and I'd want to be precise about *why*: the binding
constraint is Prometheus query volume, not optimizer CPU. Three range queries per
container per cycle, and a 7-day range at 1-minute resolution is ~10,000 samples
per series.

In the order I'd do it:

1. **Recording rules** — precompute per-container aggregates in Prometheus. Highest
   leverage by far; probably carries you to ~10,000 on its own.
2. **Informers instead of List** — discovery cost proportional to churn, not
   cluster size.
3. **Staggered analysis** — analyse a fraction per cycle. Recommendations change
   slowly.
4. **Shard by namespace** — only here does the monolith stop being adequate.

I'd resist reaching for a queue or a service mesh: neither addresses the actual
constraint.

**Why a monolith rather than separate controller and API services?**

The API serves a snapshot the controller produces, in memory, behind a mutex.
Splitting them needs a shared store and a synchronisation protocol to solve a
problem that doesn't exist. The cost is that the API can't scale independently,
which matters at a request rate far above what a dashboard generates.

I'd rather defend one process with clean package boundaries than three processes
with a distributed state problem.

---

## Cloud economics

**Why doesn't a CPU request map directly to cloud cost?**

Nodes are billed whole. Freeing 200m on a pod doesn't reduce the bill — the node
is still running and still charged. The saving is real only when freed capacity
lets the autoscaler remove a node, or lets a pending pod schedule without adding
one.

That's why I call it an *allocation-based estimate* and say explicitly that every
savings figure is an upper bound. The CLI prints that caveat every time.

**How does bin packing affect it?**

It's the reason aggregate reductions don't translate linearly. A node with 4 spare
cores and 1 spare GiB can't host a pod needing 1 core and 4 GiB. Per-workload
right-sizing is genuinely only half the problem; the other half is packing, and I
don't model it.

**How would real cloud billing change the model?**

Commitments and Spot change the effective rate by 30-70% and depend on
cluster-wide purchasing rather than per-workload usage. That's the problem Kubecost
solves, and it's harder than mine. I'd integrate a billing API rather than try to
model it.

**Where do your per-unit rates come from?**

I split an instance's hourly price 70/30 between CPU and memory. There's no ground
truth — providers sell machines, not separable resources. The construction has one
defensible property: a fully packed node costs exactly its list price, so the model
can't systematically over- or under-count in aggregate. I verify that with a test.

The split affects the *relative* weight of CPU versus memory savings. It affects
none of the reliability findings, which contain no pricing.

---

## Research method

**What's your research question?**

Not "how do I right-size Kubernetes workloads" — that's solved. It's "how would
you know whether a right-sizing recommendation was good, given that the data
available to evaluate it is censored by the decision you're evaluating?"

**What are your baselines?**

The unchanged request, plus mean, p50, p90, p95, p99 and max — each at four safety
margins. All of them run through the *same* engine code path with a different
Strategy, so a difference in results can't come from a difference in plumbing.

**What's your contribution?**

An evaluation methodology and an empirical study. Explicitly *not* a novel
algorithm — every strategy I evaluate is a standard statistic, and percentile-based
right-sizing with asymmetric CPU/memory handling is the Vertical Pod Autoscaler's
design.

What I contribute: ground-truth evaluation via counterfactual replay with
censoring modelled explicitly; reliability as a measured result rather than an
assumption; a reproducible benchmark another engine could be dropped into.

**How do you make sure the experiments test the deployed system?**

Research mode and production mode share the same `recommender.Engine` through the
same entry point. Only the data source differs — Prometheus or the simulator —
and both produce the same `model.Series` type. If the experiments ran a separate
prototype, the findings would be about that prototype.

**What would falsify your central design claim?**

The claim is that CPU and memory need different policies. It's falsified if the
best CPU strategy and the best memory strategy are the same *and* forcing a shared
strategy costs nothing measurable. I ran that as an explicit ablation.

The result is nuanced and I report it that way: separating the policies raises the
best universally-safe saving by only 1.2 percentage points in aggregate. The
substantive finding is that the two resources fail on *different workloads* — CPU
on bursty and spiky, memory on bursty-memory, growing and sawtooth — so a unified
policy must be conservative enough for the union of both failure sets.

**Why are synthetic workloads acceptable?**

They're not "acceptable" so much as necessary for this specific question. Ground
truth is unobtainable on a real cluster — that's the whole problem. The choice was
exact ground truth on synthetic demand, or no ground truth on real demand.

It's the project's biggest limitation and the first thing in my threats-to-validity
document. The honest description of my findings is "these strategies behave this
way on workloads with these structural properties", not "these strategies behave
this way". The fix is replaying against production traces, which is my top future
work item.

**What did you find that contradicted your expectations?**

Several things, which is the best evidence the experiments could produce unwelcome
results:

- My own shipped default was wrong, and I replaced it.
- The observed maximum isn't a safe bound — that surprised me most.
- A safety margin can't rescue a bad statistic, which I'd assumed it could.
- Short windows are worse on *every* axis, not a trade-off.
- One of my ablations initially measured nothing at all.

**Tell me about the ablation that measured nothing.**

The OOM protection ablation returned bit-identical results with the gate on and
off. My first instinct was a bug in the wiring.

It wasn't. Every workload in my catalog is deliberately over-provisioned, so none
of them ever OOMed, so there was no OOM history for the gate to act on. The gate
was working correctly and the *experiment* was incapable of observing it.

An ablation that can't observe the component it ablates measures nothing, and
reporting "no effect" would have been a false negative produced by my design. I
added a configuration that deploys workloads at 35% of their memory request —
creating the already-failing, censored population the gate exists for. There it
fires on 10.6% of conditions and cuts kill episodes by 12% at zero savings cost.

I report both versions, because the failure is more instructive than the fix.

**What's your weakest result?**

The unified-strategy ablation. 1.2 percentage points is close to my resolution
limit with ten seeds, so I describe it as modest rather than as a clear win. The
per-class evidence is much stronger than the aggregate, and I say so rather than
leading with the number that sounds better.

---

## Engineering

**How is the code organised?**

A modular monolith. The important boundary is `model.Series` — the single type
where data acquisition meets analysis. Strategies are pure functions of statistics;
gates are separate and can see evidence. `internal/experiment` contains no
right-sizing logic at all, which is a property I protect, because if it did the
experiments would measure code that isn't deployed.

**Tell me about a bug you found.**

A good one, caught by an assertion rather than a test. I had an invariant that a
safety gate can never lower a recommendation. It panicked mid-experiment on the
minimum-change gate cancelling a 9% proposed *increase*.

The gate was right — cancelling a marginal increase legitimately returns the
current request, which is lower than the proposal but is what the workload is
already running safely. My *invariant* was wrong. The correct statement is
`result >= min(proposed, current)`: a gate may never produce something more
aggressive than both the proposal and the status quo.

I fixed the invariant, and added the near-current increase cases my original test
had missed — which is why it passed while the engine panicked.

**How do you test something like this?**

Layered. Unit tests for the statistics, cross-checked against numpy reference
values. Property tests for invariants — gates never go below the floor, rounding
never decreases a value, percentiles are monotonic. Fake-clientset tests for
Kubernetes discovery including OOM detection and owner resolution. A fake
Prometheus source for the analysis service. Helm chart tests that render the chart
and assert on RBAC — that secrets are never granted, that `delete` is never
granted, that apply mode needs two switches. And an end-to-end test on kind with a
real load generator.

I also pin the experimental findings into tests: if someone changes the default
CPU strategy away from p99, a test fails with a message explaining which finding
that contradicts.

**What would you do differently?**

Build the ground-truth evaluation first. I wrote the engine, then the simulator,
then discovered that scoring against measured usage is close to circular. Starting
from "how will I know if this works" would have shaped the engine's interfaces
better and saved a rewrite of the evaluation.

---

## Questions to ask back

- How do you currently decide resource requests? Is it measured or inherited?
- Do you run VPA? In recommendation mode or applying?
- What does an OOMKill cost you — is it a page, or absorbed by retries?
- Is your cluster autoscaled? That determines whether right-sizing saves money at
  all or just creates headroom.
