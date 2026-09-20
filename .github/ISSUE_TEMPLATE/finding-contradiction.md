---
name: Contradicting evidence
about: You ran this against real workloads and the findings did not hold
title: "[evidence] "
labels: research
---

**This is the most valuable kind of issue for this project.** The published
findings come from synthetic workloads, which is the study's principal limitation.

## Which finding

Which result from `research/results.md` did your observation contradict?

## Workload characteristics

If you can share them:

- Burstiness (max/mean) — the API reports this under `observed.burstiness`
- Approximate peak duty cycle: how often, and for how long?
- CPU-bound, memory-bound, or mixed?
- Anything unusual: GC pauses, batch phases, cold starts

## What you observed

The recommendation JSON is ideal — it carries the statistics, the strategy, the
gates that fired and the reason:

```
curl -s localhost:8080/api/v1/recommendations/<ns>/<name> | jq
```

And what actually happened when it was applied, if it was.

## Configuration

```
curl -s localhost:8080/api/v1/policy | jq
```
