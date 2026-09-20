---
name: Bug report
about: Something behaved incorrectly
title: ""
labels: bug
---

## What happened, and what you expected

## The recommendation, if relevant

Recommendations carry their own evidence, which usually contains the answer:

```
curl -s localhost:8080/api/v1/recommendations/<ns>/<name> | jq
```

## Your policy

```
curl -s localhost:8080/api/v1/policy | jq
```

## Environment

- Kubernetes version:
- Prometheus version:
- Chart version / image tag:
- Installed via Helm / manifests / `koctl`:
