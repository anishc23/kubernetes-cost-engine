# Experimental Setup

Everything needed to reproduce the results, and a precise statement of which
results come from which method.

## Two evaluation methods, deliberately kept separate

| | Simulation study | End-to-end validation |
|---|---|---|
| **Where** | `internal/simulator` + `internal/experiment` | `kind` cluster, `test/e2e` |
| **Data source** | generative demand model | real containers via cAdvisor → Prometheus |
| **Ground truth** | exact, by construction | none available |
| **What it establishes** | how strategies behave against known demand | that the production collection path works |
| **Used for** | every finding in [results.md](results.md) | no findings; correctness of the pipeline |

They are complements, not alternatives. The simulation has ground truth and no
production fidelity; the end-to-end test has production fidelity and no ground
truth. **No result in [results.md](results.md) comes from the kind cluster**, and
no claim about Prometheus query correctness rests on the simulator.

## Hardware and software

Results in this repository were generated on:

| | |
|---|---|
| Platform | Apple Silicon (arm64), macOS |
| Go | 1.27.1 |
| Python | 3.14 (numpy 2.5, pandas 3.0, scipy 1.18, matplotlib 3.11) |
| Kubernetes (e2e) | kind v0.33, Kubernetes v1.34 |
| Helm | v4.3 |

Exact values for any given run are in its `*.provenance.json`. The experiments are
CPU-bound, pure-Go and deterministic; they do not depend on the platform, and
`TestParallelismDoesNotChangeResults` verifies that concurrency does not change
results.

## Workload catalog

Ten classes, defined generatively in `internal/simulator/catalog.go`. Parameters
below are ground truth, not measurements.

| Class | CPU baseline → peak | Duty cycle | Memory | Declared CPU / memory |
|---|---|---|---|---|
| stable-cpu | 200m, ±8% noise | — | 256Mi flat | 1000m / 1Gi |
| bursty-cpu | 80m → 1400m | 30s every 15m (3.3%) | 320Mi flat | 2000m / 1Gi |
| periodic-cpu | 150m → 1400m | 24h raised cosine | 384Mi flat | 2000m / 1Gi |
| spiky-cpu | 60m → 2400m | 30s every 4h (0.2%) | 256Mi flat | 3000m / 1Gi |
| stable-memory | 120m | — | 700Mi, ±2% | 500m / 2Gi |
| growing-memory | 140m | — | 500Mi → 2.2x over window | 500m / 2Gi |
| bursty-memory | 130m | 2m every 2h (1.7%) | 400Mi → 1500Mi | 500m / 3Gi |
| sawtooth-memory | 150m → 600m | 20m cycle | 300Mi → 1400Mi sawtooth | 800m / 3Gi |
| mixed | 200m → 1200m | 12h cycle + bursts | 500Mi → 1200Mi, correlated | 2000m / 2Gi |
| idle | 5m, ±30% | — | 40Mi | 500m / 512Mi |

### Why these parameters

**Burst structure is specified as a duty cycle, not a sample count.** The fraction
of *samples* in a burst depends on the sampling step; the duty cycle is a property
of the workload. Expressing it this way keeps the class definitions meaningful
across sampling resolutions.

**The three CPU burst classes are calibrated to span three distinct regimes**, so
that the strategy comparison can attribute a failure to peak *duration* rather
than peak *height*:

| Class | duty cycle | p95/max | p99/max | regime |
|---|---:|---:|---:|---|
| periodic-cpu | broad cosine | 0.75 | 0.82 | both percentiles see the peak |
| bursty-cpu | 3.3% | **0.06** | 0.86 | p95 misses, p99 catches |
| spiky-cpu | 0.2% | 0.03 | **0.03** | both miss |

`TestClassesExhibitTheirDefiningBehaviour` asserts these properties, so a change
that collapsed the regimes would fail the build rather than silently invalidate
the workload-class findings.

**Over-provisioning varies by class (1.1x to 48x)** because RQ1 asks how much
waste exists; a single uniform factor would make that question trivial.

## Experiment configurations

| Config | Conditions | Purpose |
|---|---:|---|
| `main.yaml` | 19,600 | RQ1–RQ5: strategies x classes x margins |
| `sensitivity.yaml` | 12,800 | RQ5: fine margin sweep, CPU and memory decoupled |
| `windows.yaml` | 3,600 | RQ6, RQ7: window length and stability |
| `ablation_no_oom_protection.yaml` | 3,600 | Ablation A (first attempt; measured nothing) |
| `ablation_no_gates.yaml` | 3,200 | Ablations B, D, E: raw statistics only |
| `ablation_unified_strategy.yaml` | 2,000 | Ablation C: one shared strategy |
| `oom_recovery.yaml` | 1,600 | Ablation A, corrected: already-failing population |
| `oom_recovery_no_gate.yaml` | 1,600 | its paired control |
| `percentile_method.yaml` | 1,200 | estimator sensitivity |
| `validate_default.yaml` | 600 | confirmatory run of the revised default |
| `smoke.yaml` | 48 | fast pipeline check; not a source of results |

**48,248 scored conditions**, each averaging 10 (or 5) independent trace
realisations evaluated on a held-out horizon.

Total runtime is under two minutes on a laptop, which is a deliberate property:
an experiment suite that takes hours is one nobody re-runs after changing the
engine, and results that are not re-run drift out of correspondence with the code.

## Trace geometry

```
main / ablations:  96h trace, 30s step   = 11,521 samples
                   72h fitting window + 24h held-out horizon

windows:          168h trace, 30s step   = 20,161 samples
                   up to 72h window + 12h horizon + 8 x 12h stability slides
```

30 seconds matches a common Prometheus scrape interval and is short enough to
represent the 30-second bursts in the catalog. It is also the resolution limit of
the study: shorter excursions are not representable (see
[limitations.md](limitations.md)).

## Reproducing

```bash
make experiments      # all configs -> experiments/results/  (~2 min)
make analysis         # all figures and tables               (~30 s)

# or one at a time
go run ./cmd/experiment -config experiments/configs/main.yaml -dry-run   # size it first
go run ./cmd/experiment -config experiments/configs/main.yaml
```

Each run writes three files: `<name>.csv` (flat, for pandas), `<name>.json`
(archival, with provenance attached) and `<name>.provenance.json` (run metadata
alone).

### What provenance records

Git commit and whether the tree was dirty; a content hash of the resolved
configuration; tool version, Go version, OS and architecture; the cost model;
start and end timestamps; and both the intended condition count and the actual
record count, so that silently skipped conditions would be visible.

A run from a dirty working tree is marked `git_dirty: true` and logs a warning:
such results cannot be reproduced from the recorded commit alone, and a reader
deserves to know that rather than discovering it later.

## End-to-end validation on kind

```bash
make kind-up          # cluster + Prometheus + demo workloads
make e2e              # Kubernetes -> Prometheus -> optimizer -> API
make kind-down
```

This deploys the real load generator (`cmd/workload-gen`), which burns real CPU
and allocates real memory to the same generative schedule as the simulator, then
verifies that the optimizer discovers the workloads, retrieves their metrics
through the production Prometheus queries, and serves recommendations.

It establishes that the collection path is correct. It cannot establish whether a
recommendation was *good*, because the cluster provides no ground truth — which is
the whole reason the simulation study exists.
