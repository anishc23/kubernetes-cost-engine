# Cost Model

## What this computes

An **allocation-based cost estimate**: the price of the CPU and memory a workload
*reserves*, at a configured per-unit rate, for a configured number of hours.

```
monthly cost = (cores × $/core-hour + GiB × $/GiB-hour) × 730 × replicas
```

730 hours is the mean month (8760/12), which is the basis cloud providers quote
monthly prices on.

## What this is not

**It is not a cloud bill, and the gap is large.** A cost tool that overstates its
own accuracy is worse than one that reports a wider but honest number, so the
limitations are listed before the features.

### Nodes are billed whole

This is the big one. Reducing a pod's request frees capacity on a node that is
still running and still charged. The saving becomes real only when the freed
capacity lets the cluster autoscaler remove a node, or lets a pending pod schedule
without adding one.

Freeing 200m across twenty pods saves **nothing** unless those 4 cores allow one
fewer node. **Every savings figure this tool produces is an upper bound.**

### Bin packing is lossy

A node with 4 spare cores and 1 spare GiB cannot host a pod needing 1 core and 4
GiB. Real clusters strand capacity in ways a per-workload model cannot see, so
even a large aggregate reduction may not translate into a node count reduction.

### Commitments change the rate by large factors

Reserved Instances, Savings Plans, committed-use discounts, Spot and sustained-use
discounts routinely change the effective rate by 30–70%, and they depend on
cluster-wide commitments rather than per-workload usage. The model uses one flat
rate.

### Not modelled at all

Managed control plane fees, storage, network egress, load balancers, the cost of
the monitoring stack this tool depends on.

## Where the rates come from

Per-unit rates are derived by splitting an instance's hourly price between its CPU
and memory:

```
$/core-hour  = instance_price × cpu_share      / vCPU
$/GiB-hour   = instance_price × (1 - cpu_share) / GiB
```

With the default `cpu_share = 0.70`, an m5.xlarge at $0.192/h (4 vCPU, 16 GiB)
gives $0.0336/core-hour and $0.0036/GiB-hour.

This construction has one property worth stating: **a fully packed node costs
exactly its list price**, so the model cannot systematically over- or under-count
in aggregate. It is verified by a test.

### The split is an assumption

There is no ground truth for it. A cloud provider sells a machine, not separable
CPU and memory.

0.70 is used because across general-purpose instance families the marginal price
of a vCPU is consistently several times that of a GiB, and a 70/30 split
reproduces published per-unit rates of CPU-optimised versus memory-optimised
families to within roughly a factor of two. **That is a weak justification and is
treated as one**: the parameter is configurable and varied in the sensitivity
analysis.

What it affects: the *relative* weight of CPU and memory savings, and therefore
which resource a strategy appears to help most. What it does not affect: any
reliability finding, which contains no pricing at all.

## Reading the numbers correctly

**Comparative claims are robust.** "Strategy A saves 20% more than strategy B
under an identical model" survives any pricing assumption, because the assumption
cancels. Every comparison in [research/results.md](../research/results.md) is of
this kind.

**Absolute claims are not.** "This will save $40,000/month" depends on the rate
being right, on the freed capacity being packable, and on the autoscaler acting
on it.

The CLI and the API both print the caveat alongside every savings figure, and the
summary endpoint carries it as a field, because a number presented without it
invites being read as a bill.

## Configuration

Derive from a catalog instance:

```yaml
cost:
  instanceType: m5.xlarge     # or c5.2xlarge, r5.2xlarge, n2-standard-8, ...
  cpuCostShare: 0.70
```

Or state your actual effective rate, which takes precedence — an operator who has
computed their real blended rate knows something the catalog does not:

```yaml
cost:
  cpuHourUsd: 0.021           # e.g. after a 3-year commitment
  memoryGibHourUsd: 0.0028
  provider: aws
  region: us-east-1
```

Inspect the catalog and the derived rates:

```bash
koctl pricing
```

### The bundled prices are dated, not live

Every catalog entry records the date its price was captured, so a reader can tell
how stale it is. An undated hard-coded price cannot be assessed at all. A
deployment that cares about accuracy should configure its own rates.

## Comparison with Kubecost and OpenCost

These solve a **different and substantially harder problem**: cost *allocation*.
Given a real cloud bill, who spent it? That requires billing API integration,
commitment amortisation, Spot pricing, and multi-cluster aggregation. Their
numbers are grounded in invoices.

This model exists only to make right-sizing decisions comparable — to answer "is
this recommendation worth the rollout?" — and is deliberately simple enough to
audit in one page.

**A team that wants to know what their cluster costs should use Kubecost or
OpenCost.** No claim to the contrary is made here.

## Worked example

```
Workload: api-server, 3 replicas
Current:     cpu 2000m, memory 4Gi
Recommended: cpu 520m,  memory 975Mi
Model:       m5.xlarge / us-east-1 → $0.0336/core-h, $0.0036/GiB-h

Current per replica:  2.0 × 0.0336 + 4.0 × 0.0036 = $0.0816/h → $59.57/mo
Recommended:          0.52 × 0.0336 + 0.952 × 0.0036 = $0.0209/h → $15.26/mo

Saving: $44.31/mo per replica × 3 = $132.93/mo  (74%)
```

**What that figure means:** if this reduction, combined with others, lets the
cluster run fewer m5.xlarge nodes, up to $132.93/month is recoverable. If the
cluster is running the same nodes tomorrow, the realised saving is zero and the
benefit is headroom rather than money.
