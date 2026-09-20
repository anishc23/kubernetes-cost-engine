# Limitations

[threats_to_validity.md](threats_to_validity.md) covers what could make the
*findings* wrong. This document covers what the *system* does not do, so that a
reader evaluating it for use knows the boundaries before deploying it.

## The engine

**It does not right-size limits.** Only requests are recommended and, in apply
mode, only requests are patched. A memory limit determines when the kernel kills
the process; nothing in a usage series justifies changing that automatically.
Joint request/limit optimisation is a genuinely different problem, requiring a
model of burst tolerance the engine does not have.

**It does not consider the cluster.** Each workload is right-sized in isolation.
The engine cannot see that shrinking one pod strands capacity on a node, or that
twenty small reductions would let the autoscaler remove a node while nineteen
would not. Cluster-level bin packing is where allocation savings become real
savings, and it is out of scope.

**It does not right-size init containers.** Their requests affect scheduling — a
pod's effective request is the maximum of init and the sum of app requests — but
they run briefly and their usage series is almost entirely empty. Sizing them from
a percentile would be confident advice derived from no data.

**It does not handle vertical autoscaling interaction.** Running this alongside the
Vertical Pod Autoscaler would produce two controllers writing the same field. The
engine does not detect VPA ownership, and deploying both in apply mode on the same
workload would be a mistake the tool does not currently prevent.

**It has no notion of a workload's importance.** A batch job and a payment service
receive identical treatment given identical usage. In practice the acceptable risk
differs by orders of magnitude. The engine exposes risk levels so an operator can
apply that judgement, but it cannot apply it itself.

**Its workload classification is a research-mode construct.** In experiments the
class is ground truth from the generator. In production the engine computes
burstiness and reports it, but does not classify workloads, and the per-class
policy recommendations in [results.md](results.md) therefore cannot be applied
automatically. Doing so would require a classifier whose errors would have to be
characterised before it could be trusted — deliberately not built, since an
interpretable engine that declines is more useful than an opaque one that guesses.

## The cost model

**It is an allocation-based estimate, not a bill.** It prices reserved capacity at
a configured rate. It does not model node granularity, bin packing, the cluster
autoscaler, Reserved Instances, Savings Plans, Spot, sustained-use discounts, the
managed control plane, storage, or network egress. Every savings figure it
produces is an upper bound. [docs/cost-model.md](../docs/cost-model.md) has the
full treatment.

**Its per-unit rates rest on an assumption.** Splitting an instance price between
CPU and memory has no ground truth, since providers sell machines. The shipped
70/30 split reproduces published per-unit rates across instance families to within
roughly a factor of two, which is a weak justification and is treated as one.

**Its bundled price list is dated, not live.** Prices are captured on a stated date
so a reader can tell how stale they are. A deployment that cares about accuracy
should configure its own rates.

## The measurement path

**Prometheus resolution bounds what is visible.** At a one-minute step with a
four-minute rate window, CPU bursts shorter than the rate window are smoothed away
and may not appear in any percentile. A workload whose peaks are sub-scrape can be
recommended a request it will exceed regularly, and the engine has no way to know.

**`rate()` smooths by design.** `rate()` over `container_cpu_usage_seconds_total`
is used rather than `irate()` because `irate()` amplifies scrape jitter into
apparent spikes over multi-day windows. The cost is that genuine short spikes are
attenuated, which biases percentile policies toward under-provisioning for bursty
workloads — in the same direction as the finding in RQ2, and therefore a possible
partial cause of it on real clusters.

**Working set is not resident set.** `container_memory_working_set_bytes` excludes
reclaimable page cache, which is correct for OOM prediction since it is what the
kernel compares against the limit. It will nonetheless read lower than what `top`
reports inside the container, which surprises operators.

**Gaps are detected but not repaired.** The coverage check catches series with
missing samples and reports `INSUFFICIENT_DATA`. It does not interpolate or
attempt to distinguish "the exporter was down" from "the workload did not exist" —
both are treated as absence of evidence.

## Scale

**Tested at tens of workloads, not thousands.** Discovery lists once per namespace
and queries Prometheus once per container per resource. At ten thousand workloads
that is a large query burst on every cycle.
[docs/architecture.md](../docs/architecture.md) sets out what would have to change
(recording rules, informers, sharding) — as a design, not as an implementation.
The current code has not been run at that scale and no performance claim is made
about it.

**Snapshots are in memory.** Results do not survive a restart; the first cycle
after startup re-derives everything. There is no persistence and no history of
past recommendations, so recommendation drift over weeks is not observable in
production the way it is in the research framework.

## The experiments

**Ten seeds per condition.** Sufficient for interval estimation of medians, not
for resolving small effects. The ablation results in particular sit close to that
resolution.

**Workloads are evaluated independently.** No correlated demand across workloads,
so no cluster-level risk aggregation. If real bursts are correlated — and they
usually are — the reported per-workload risk understates cluster-level risk.

**One container per synthetic workload.** Multi-container pods, where one
container's request affects another's scheduling, are supported by the engine and
not exercised by the experiments.

**No failure injection.** Node failures, evictions, network partitions and
Prometheus outages are handled in code
([docs/failure-modes.md](../docs/failure-modes.md)) and are not exercised
experimentally.

## Honest summary

This is a research prototype with a production-shaped implementation. The
engineering is real — tested, instrumented, deployable — and the empirical work is
real, reproducible, and includes results that contradicted the author's initial
design. What it is not is a system validated in production, and the strongest
claim the evidence supports is:

> On controlled synthetic workloads with known ground truth, these right-sizing
> strategies behave in these measurable ways, and the differences between them are
> large enough to matter operationally.
