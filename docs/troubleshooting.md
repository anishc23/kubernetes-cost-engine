# Troubleshooting

## Every workload says INSUFFICIENT_DATA

By far the most common problem. The engine is telling you it cannot see enough
usage history — the reason field says exactly what is missing.

```bash
curl -s localhost:8080/api/v1/recommendations \
  | jq -r '.recommendations[].containers[].cpu | "\(.samples) samples: \(.reason)"' | head
```

Work through these in order:

**1. Is Prometheus the right one?**

```bash
curl -s localhost:8080/readyz | jq
```

If `prometheus` is not `ok`, the address is wrong or unreachable. This is the
cause more often than anything else.

**2. Is cAdvisor being scraped?**

```bash
curl -s 'http://prometheus:9090/api/v1/query?query=count(container_cpu_usage_seconds_total)' | jq
```

Zero results means the metrics the engine needs are not in Prometheus at all.
Check that your Prometheus scrapes the kubelet's `/metrics/cadvisor` endpoint, and
that `metric_relabel_configs` is not dropping `container_*` series.

**3. Do the pod names match?**

The engine builds an anchored regex per workload kind. Verify it matches:

```bash
curl -s 'http://prometheus:9090/api/v1/query?query=count(container_cpu_usage_seconds_total{namespace="prod",pod=~"^api-[a-z0-9]+-[a-z0-9]{5}$"})' | jq
```

Non-standard pod naming — from a custom controller, or an operator-managed
workload — will not match.

**4. Has enough time passed?**

`minSamples: 60` at `step: 1m` needs 60 minutes of history. A freshly installed
optimizer on a freshly created workload legitimately has nothing to say. For a
quick local check, lower the thresholds:

```bash
--set policy.minSamples=10 --set policy.minDuration=5m --set policy.observationWindow=30m
```

**5. Is coverage low?**

```bash
curl -s localhost:8080/api/v1/recommendations \
  | jq '.recommendations[].containers[].cpu.reason' | grep coverage
```

Low coverage means the series spans the window but has gaps — scrape failures,
pod churn, or Prometheus restarts.

## No workloads discovered at all

```bash
curl -s localhost:8080/api/v1/workloads | jq '.count'
```

- **System namespaces are excluded by default** (`kube-system`, `kube-public`,
  `kube-node-lease`). This is intentional.
- **DaemonSets are excluded by default.** Set
  `kubernetes.includeDaemonSets=true` if you want them.
- **RBAC.** Check for permission errors:
  ```bash
  kubectl logs deploy/cost-optimizer | grep -i "namespace discovery failed"
  ```
- **A label selector** in `kubernetes.labelSelector` may be filtering everything
  out.

## Recommendations look far too aggressive

Check what the engine observed, not just what it recommended:

```bash
curl -s localhost:8080/api/v1/recommendations/prod/api | jq '.containers[0].cpu'
```

If `observed.max` is much lower than you expect, the usage is probably being
smoothed away. The CPU query uses `rate()` over a 4×step window, so bursts
shorter than that are attenuated. Lower `prometheus.step` to see finer structure —
at the cost of more samples per query.

If the workload is bursty, the burstiness figure will show it. Above the
`maxCpuBurstiness` threshold the engine withholds the reduction on its own.

## Memory recommendations are always BLOCKED

This is the OOM protection gate, and it is usually correct:

```bash
curl -s localhost:8080/api/v1/recommendations \
  | jq -r '.recommendations[] | select(.containers[].memory.decision=="BLOCKED")
           | "\(.namespace)/\(.name): \(.containers[0].memory.reason)"'
```

Two distinct causes:

- **"OOMKill(s) observed"** — the workload has been killed recently. The engine is
  right to refuse: its working-set series is censored at the limit and understates
  what it needs. Fix the workload, not the optimizer.
- **"no restart or OOM evidence available"** — pod listing failed, or every pod was
  recreated recently so no termination history exists. Check RBAC on pods.

To study the effect, `policy.oomRequireEvidence=false` relaxes the second case
only. Do not disable `oomProtection` on a production cluster.

## Savings look implausibly large

They probably are — as *realisable* savings. The cost model is allocation-based
and is an upper bound: it prices reserved capacity, not your bill. A large
identified saving becomes real money only when the freed capacity lets the cluster
run fewer nodes. See [cost-model.md](cost-model.md).

Also check `cost.instanceType` matches your actual node shape; deriving rates from
an m5.xlarge when you run r5.4xlarge will misprice memory substantially.

## The API returns an empty list but /readyz is OK

Readiness requires a completed cycle, so this means the cycle completed and found
nothing. Check the discovery count first (`/api/v1/workloads`), then the filters
above.

## Analysis is slow

```bash
curl -s localhost:9090/metrics | grep optimizer_analysis_duration_seconds_sum
```

The cost is dominated by Prometheus round trips, not optimizer CPU. In order of
effectiveness:

1. Increase `analysis.interval` — recommendations change slowly.
2. Increase `prometheus.step` — fewer samples per query.
3. Add Prometheus recording rules for the per-container aggregates.
4. Increase `analysis.concurrency` — but this raises load on Prometheus, which is
   usually the bottleneck, so it can make things worse.

## Pod will not start

```bash
kubectl describe pod -l app.kubernetes.io/name=k8s-cost-optimizer
```

- **CreateContainerConfigError** — usually the read-only root filesystem without
  the `/tmp` mount. The chart provides it; a hand-written manifest may not.
- **CrashLoopBackOff** — check the logs; configuration validation reports every
  problem at once on startup.
- **ImagePullBackOff** — in kind, the image must be loaded:
  `kind load docker-image <image> --name <cluster>`.

## Helm install fails at render time

This is intentional for the dangerous cases:

- `analysis.apply=true` without `rbac.allowApply=true` — enabling mutation requires
  both, so a single typo cannot do it.
- A safety factor below 1.0 — this would recommend less than the statistic
  identified as the safe envelope.
- An empty `prometheus.address` — the optimizer cannot work without it.

The error message names the setting and the constraint.

## Experiments fail to run

```bash
go run ./cmd/experiment -config experiments/configs/main.yaml -dry-run
```

- **"trace_duration is shorter than the longest observation window plus the
  evaluation horizon"** — the longest-window condition would be fitted on truncated
  data. Lengthen `trace_duration`.
- **A config key appears to be ignored** — YAML keys come from the `json` tags
  (snake_case). `sigs.k8s.io/yaml` converts YAML to JSON and ignores `yaml:` tags
  entirely. `TestShippedConfigsLoad` catches this for shipped configs.

## Figures will not regenerate

```bash
make analysis
```

- **FileNotFoundError** — run `make experiments` first.
- **Missing Python packages** — `make venv` creates the environment.

## Getting more detail

```bash
# every field the engine used for one decision
curl -s localhost:8080/api/v1/recommendations/prod/api | jq

# the exact policy in force, including its content hash
curl -s localhost:8080/api/v1/policy | jq

# which safety gates are firing across the cluster
curl -s localhost:9090/metrics | grep optimizer_gates_fired_total

# verbose logs
--set log.level=debug
```

Every recommendation carries its reason, the statistics behind it, the strategy
and margin used, and which gates fired. If a decision is surprising, that response
usually contains the explanation without needing logs at all.
