# Portfolio Description

Ready-to-use descriptions at several lengths. Every number is measured.

---

## One line

> A Kubernetes resource right-sizing engine, and a ground-truth study showing that
> evaluating such systems on cost savings alone reverses which strategy looks best.

---

## Short (GitHub description, ~250 characters)

> Kubernetes resource right-sizing with resource-specific CPU/memory policies and
> OOM-aware safety gates, plus a reproducible ground-truth study of 49,800
> conditions showing that reliability constraints reverse the strategy ranking.

---

## Medium (portfolio card, ~150 words)

> **Kubernetes Cost Optimization Engine** · Go, Python, Kubernetes, Prometheus
>
> Kubernetes reserves capacity by declared request, not usage, so clusters carry
> substantial idle capacity. Tools exist to fix this. I got interested in a prior
> question: how would you know whether a right-sizing recommendation was *good*?
>
> It turns out you mostly can't from production data, because usage is censored by
> the configuration you're evaluating — throttled CPU is never recorded as used,
> and an OOMKilled container never records the memory it was reaching for.
>
> So I built a generative workload simulator with known ground truth and scored
> recommendations against demand the engine never saw. Across 49,800 conditions,
> adding a reliability constraint *reverses* the ranking of strategies — and my own
> default policy turned out to leave 34% of demanded CPU work unserved on bursty
> workloads, so I replaced it with one the data supported.

---

## Long (personal site, ~400 words)

> **Kubernetes Cost Optimization Engine**
>
> Kubernetes schedules workloads against declared resource requests rather than
> observed usage. Operators over-declare for a rational reason — under-declaring
> fails immediately and visibly, over-declaring fails slowly and on someone else's
> budget — and the result is a lot of reserved capacity sitting idle.
>
> Several tools correct this by recommending a high percentile of historical usage.
> I built one too, but the part I found genuinely interesting was a prior question:
> **how would you know whether a recommendation was good?**
>
> That question is harder than it looks. The usage data available to evaluate a
> recommendation is *censored by the configuration being evaluated*. If a CPU
> request is too low, the container is throttled, so measured usage sits at the
> request rather than above it — the evaluation reports perfect utilisation for a
> configuration that degraded the service. If a memory limit is too low, the
> container is killed, and its working-set series tops out at exactly the limit that
> killed it. The measurement is biased, not merely noisy, and it is biased worst in
> the cases where the configuration is wrong.
>
> So I built the evaluation around a generative workload simulator. The engine sees
> only a censored observation of a training window; its recommendation is then
> replayed against uncensored demand over a held-out period it never saw. Ten
> workload classes, calibrated so their peak duty cycles span three distinct regimes
> relative to p95 and p99. The production system and the research framework run the
> same recommendation engine, so the findings are about the deployed code.
>
> Across 49,800 scored conditions, several results surprised me. Adding a
> reliability constraint *reverses* the strategy ranking — mean-based policies lead
> on raw savings and deliver zero savings that survive the constraint. No percentile
> below the maximum is universally safe, because the safe percentile depends on the
> workload's duty cycle, which the strategy can't see. The observed maximum isn't an
> upper bound: sizing memory to it survived only 47% of held-out periods. And a
> safety margin can't rescue a statistic that structurally excludes the peak.
>
> Most usefully, my own shipped default was refuted by my own experiments — p95 for
> CPU left 34% of demanded work unserved on bursty workloads — so I derived a
> replacement from the data and validated it separately. One of my ablations also
> turned out to be measuring nothing at all, and I report that too, because a
> project whose experiments only ever confirm its design isn't measuring the design.
>
> Go, Python, client-go, Prometheus, Helm, kind. Tested at five levels, end-to-end
> validated on a real cluster.

---

## For a conversation

Open with the problem, not the tool:

> "Kubernetes reserves capacity by what you *declare*, not what you *use*, so
> clusters end up carrying a lot of idle reserved capacity. There are tools that fix
> that by looking at usage history and recommending a percentile.
>
> What got me interested was a prior question — how do you know if the
> recommendation was any good? And it turns out you mostly can't, because the data
> you'd evaluate it with is censored by the thing you're evaluating..."

Then let them ask. The interesting material is in
[`docs/interview-guide.md`](../docs/interview-guide.md).
