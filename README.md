# Kubernetes Cost Optimization Engine

> A lightweight Kubernetes resource right-sizing and cost analysis engine that compares declared resource requests with observed usage and generates workload-level optimization recommendations.

## Overview

Kubernetes workloads declare resource requests before they are scheduled.

For example:

```yaml
resources:
  requests:
    cpu: "1"
    memory: "2Gi"
```

These requests influence how cluster capacity is allocated.

In practice, workloads are often over-provisioned because operators prefer to reserve more capacity than risk performance problems. Across many workloads, this can result in significant unused reserved capacity.

This project analyzes Kubernetes workloads over a configurable observation window and answers three questions:

1. **How much CPU and memory is requested?**
2. **How much is actually used?**
3. **What resource configuration may reduce waste while maintaining an appropriate safety margin?**

The engine then estimates the potential financial impact using configurable cloud pricing information.

---

# Core Idea

A naive right-sizing system would apply the same percentile rule to every resource.

This project does not.

CPU and memory behave differently and therefore require different recommendation strategies.

## CPU

CPU is generally compressible.

If a workload wants more CPU than is available, it may be throttled or slowed rather than immediately terminated.

A CPU recommendation can therefore use high-percentile observed usage with an explicit configurable safety margin.

Conceptually:

```text
recommended_cpu =
    high_percentile_cpu_usage × cpu_safety_factor
```

The exact percentile and safety factor are configurable.

---

## Memory

Memory is treated more conservatively.

Memory pressure can result in workload termination, including OOM-related failures.

A memory recommendation therefore considers:

* Maximum observed working-set usage
* Configurable safety margin
* OOM-related restart history

Conceptually:

```text
recommended_memory =
    maximum_observed_memory × memory_safety_factor
```

If the workload has evidence of OOM-related failures during the observation period, the engine can refuse to recommend a memory reduction.

> Resource recommendations are guidance, not guarantees. Production changes should be validated through workload-specific testing and rollout controls.

---

# Features

* Kubernetes cluster inspection
* Pod and workload resource request discovery
* CPU usage analysis
* Memory usage analysis
* Prometheus integration
* Configurable observation windows
* CPU right-sizing recommendations
* Memory right-sizing recommendations
* OOM-aware memory reduction protection
* Per-workload waste estimation
* Configurable cloud pricing model
* Monthly cost projection
* REST API
* Grafana dashboard
* Kubernetes-native deployment
* Helm chart installation
* Local `kind` or `k3d` development support
* Deliberately over-provisioned demo workloads for validation

---

# Architecture

```text
                    ┌──────────────────────────┐
                    │      Kubernetes API      │
                    │       client-go          │
                    └────────────┬─────────────┘
                                 │
                                 ▼
                    ┌──────────────────────────┐
                    │   Cluster Data Collector │
                    │                          │
                    │ Pods / Containers        │
                    │ CPU Requests             │
                    │ Memory Requests          │
                    │ Restart Information      │
                    └────────────┬─────────────┘
                                 │
                                 │
                                 ▼
┌──────────────────┐   ┌──────────────────────────┐
│    Prometheus    │──▶│   Usage Data Collector   │
│                  │   │                          │
│ CPU Usage        │   │ Historical CPU Usage     │
│ Memory Working   │   │ Historical Memory Usage  │
│ Set              │   └────────────┬─────────────┘
└──────────────────┘                │
                                    ▼
                         ┌──────────────────────────┐
                         │ Recommendation Engine    │
                         │                          │
                         │ CPU Strategy             │
                         │ Memory Strategy          │
                         │ OOM Safety Checks        │
                         └────────────┬─────────────┘
                                      │
                                      ▼
                         ┌──────────────────────────┐
                         │       Cost Engine        │
                         │                          │
                         │ Requested Capacity      │
                         │ Observed Usage          │
                         │ Potential Waste         │
                         │ Monthly Projection      │
                         └────────────┬─────────────┘
                                      │
                         ┌────────────┴─────────────┐
                         ▼                          ▼
               ┌──────────────────┐       ┌──────────────────┐
               │    REST API      │       │ Grafana Dashboard│
               └──────────────────┘       └──────────────────┘
```

---

# Components

## 1. Kubernetes Controller / Collector

Written primarily in Go using Kubernetes client libraries.

The collector discovers:

* Namespaces
* Pods
* Containers
* Resource requests
* Resource limits
* Workload metadata
* Restart information

The initial objective is to reliably produce:

```text
WORKLOAD | CPU REQUESTED | CPU OBSERVED | MEMORY REQUESTED | MEMORY OBSERVED
```

---

## 2. Prometheus Usage Analysis

Historical usage is collected through Prometheus queries.

Relevant metrics may include:

```text
container_cpu_usage_seconds_total
container_memory_working_set_bytes
```

Queries are evaluated over a configurable observation window, such as seven days.

The recommendation engine can calculate:

* Average usage
* Maximum usage
* Configured percentile usage
* Peak-to-request ratio
* Sustained underutilisation

Metric availability and exact label structures depend on the Prometheus and Kubernetes environment.

---

## 3. Recommendation Engine

The recommendation engine evaluates CPU and memory independently.

### CPU Recommendation

CPU recommendations use a configurable high-percentile usage estimate plus safety margin.

Example concept:

```text
Observed p95 CPU usage: 0.30 cores
Safety factor: 1.20

Recommendation:
0.30 × 1.20 = 0.36 cores
```

The engine may round recommendations to practical Kubernetes quantities.

---

### Memory Recommendation

Memory recommendations are more conservative.

Example concept:

```text
Maximum observed memory: 700 MiB
Safety factor: 1.25

Recommendation:
875 MiB
```

If OOM-related events are detected during the observation period, memory reductions can be blocked or flagged for manual review.

---

# Cost Model

The cost model estimates the cost associated with reserved cluster capacity.

Conceptually:

```text
Monthly Cost =
    Requested CPU Capacity × CPU Cost Rate
    +
    Requested Memory Capacity × Memory Cost Rate
```

Potential waste is estimated by comparing:

```text
Current requested resources
        vs
Recommended resources
```

Example output:

```text
Workload: checkout-service

Current:
CPU Request:      2.00 cores
Memory Request:   4 GiB

Recommended:
CPU Request:      0.75 cores
Memory Request:   1.5 GiB

Estimated Potential Savings:
$XX.XX per month
```

Actual cloud billing depends on many factors, including:

* Node instance types
* Cluster autoscaling behavior
* Bin packing efficiency
* Reserved instances or savings plans
* Spot instances
* Region
* Managed Kubernetes pricing
* Networking and storage costs

For this reason, cost estimates are presented as configurable estimates rather than exact cloud bills.

---

# REST API

The engine exposes workload analysis through a REST API.

Planned example endpoints:

```text
GET /api/v1/workloads
GET /api/v1/workloads/{namespace}/{name}
GET /api/v1/recommendations
GET /api/v1/cost-summary
GET /api/v1/health
```

Example recommendation response:

```json
{
  "workload": "checkout-service",
  "namespace": "production",
  "current": {
    "cpu": "2",
    "memory": "4Gi"
  },
  "recommended": {
    "cpu": "750m",
    "memory": "1536Mi"
  },
  "estimated_monthly_savings": 0,
  "memory_reduction_allowed": true
}
```

The values above are illustrative only.

---

# Grafana Dashboard

The Grafana dashboard is intended to display:

* Current resource requests
* Actual CPU usage
* Actual memory usage
* Request-to-usage ratios
* Potential over-provisioning
* Estimated workload cost
* Estimated optimization opportunity
* CPU recommendations
* Memory recommendations
* Workloads requiring manual review

The dashboard is the primary visualization layer. No separate application web interface is required.

---

# Helm Installation

The project is packaged as a Helm chart.

The intended installation workflow is:

```bash
helm install k8s-cost-optimizer ./charts/k8s-cost-optimizer
```

A production-ready chart will include configuration for:

* Prometheus endpoint
* Observation window
* CPU percentile
* CPU safety margin
* Memory safety margin
* Pricing configuration
* Namespace scope
* API configuration

---

# Project Structure

```text
.
├── cmd/
│   ├── controller/
│   └── api/
│
├── internal/
│   ├── kubernetes/
│   ├── prometheus/
│   ├── recommendation/
│   │   ├── cpu/
│   │   └── memory/
│   ├── cost/
│   └── api/
│
├── pkg/
│   └── models/
│
├── configs/
│
├── charts/
│   └── k8s-cost-optimizer/
│
├── dashboards/
│
├── deploy/
│   ├── kind/
│   └── demo-workloads/
│
├── scripts/
│
├── docs/
└── README.md
```

The exact structure may evolve during implementation.

---

# Quick Start

## Prerequisites

* Go
* Docker
* Kubernetes
* `kubectl`
* `kind` or `k3d`
* Helm
* Prometheus
* Grafana

Apple Silicon users should ensure container images support `arm64`.

---

## Create a Local Cluster

Using `kind`:

```bash
kind create cluster --name cost-optimizer
```

Verify:

```bash
kubectl get nodes
```

---

## Deploy Prometheus

Prometheus must be configured to collect container resource metrics.

The exact deployment instructions will be added as the project implementation is completed.

---

## Deploy Demo Workloads

The repository will include intentionally over-provisioned workloads.

For example:

```yaml
resources:
  requests:
    cpu: "2"
    memory: "2Gi"
```

The workload can be designed to use substantially less capacity, allowing the recommendation engine to identify the difference.

---

## Run the Collector

```bash
go run ./cmd/controller
```

Expected high-level output:

```text
Collecting Kubernetes workload data...
Querying Prometheus usage history...
Generating recommendations...
Calculating estimated cost impact...
```

---

# Recommendation Safety

Resource optimization can cause outages if performed blindly.

The engine therefore treats recommendations as **advisory**.

Important safeguards include:

* Separate CPU and memory strategies
* Configurable safety margins
* Historical observation windows
* OOM-aware memory recommendations
* Manual review flags
* No automatic modification of workload manifests by default

Future versions may support controlled recommendation application, but automatic mutation is deliberately out of scope for the initial version.

---

# Validation Methodology

The project will be validated using controlled workloads with deliberately inflated resource requests.

The evaluation process is:

```text
1. Deploy workload with known over-provisioning
             ↓
2. Generate controlled CPU and memory behavior
             ↓
3. Collect historical metrics
             ↓
4. Run recommendation engine
             ↓
5. Compare recommendation against known workload behavior
             ↓
6. Validate stability using the recommended configuration
```

This provides a reproducible demonstration of the system rather than relying solely on static screenshots.

---

# Example Analysis

The following example is illustrative and not a measured result:

| Workload    | CPU Requested | CPU Observed | Memory Requested | Memory Observed | Recommendation          |
| ----------- | ------------: | -----------: | ---------------: | --------------: | ----------------------- |
| API Service |       2 cores |    0.3 cores |            4 GiB |         900 MiB | Reduce after validation |
| Worker      |        1 core |    0.8 cores |            2 GiB |         1.8 GiB | Keep near current       |
| Batch Job   |       4 cores |     variable |            8 GiB |        variable | Manual review           |

Actual results will be generated by the implemented system.

---

# Positioning

This project is inspired by the broader problem addressed by Kubernetes cost-management platforms and tools such as Kubecost.

The goal is **not** to reproduce a commercial platform.

Instead, this project focuses on a narrower open-source engineering problem:

> **A lightweight, Kubernetes-native engine for resource right-sizing recommendations with resource-specific safety strategies and transparent cost estimation.**

Key areas of focus include:

* Simplicity
* Transparency
* Reproducibility
* Local development
* Kubernetes-native deployment
* Clear recommendation logic

---

# Roadmap

## Cluster Discovery

* [ ] Initialize Go project
* [ ] Connect to Kubernetes using `client-go`
* [ ] List pods and containers
* [ ] Extract CPU requests
* [ ] Extract memory requests
* [ ] Extract resource limits
* [ ] Collect restart information

## Prometheus Integration

* [ ] Connect to Prometheus
* [ ] Query CPU usage
* [ ] Query memory working-set usage
* [ ] Support configurable observation windows
* [ ] Aggregate historical metrics

## Recommendation Engine

* [ ] CPU percentile-based recommendations
* [ ] CPU safety margins
* [ ] Memory peak-based recommendations
* [ ] Memory safety margins
* [ ] OOM-aware reduction protection
* [ ] Recommendation confidence / review flags

## Cost Engine

* [ ] Resource cost model
* [ ] Monthly cost projection
* [ ] Per-workload waste estimates
* [ ] Configurable pricing source
* [ ] Pricing provider abstraction

## Productization

* [ ] REST API
* [ ] Grafana dashboard
* [ ] Demo workloads
* [ ] Docker images
* [ ] Helm chart
* [ ] `kind` deployment
* [ ] End-to-end demo
* [ ] Documentation

---

# Technology Stack

| Component              | Technology                        |
| ---------------------- | --------------------------------- |
| Primary Language       | Go                                |
| Kubernetes Integration | client-go                         |
| Metrics                | Prometheus                        |
| Visualization          | Grafana                           |
| Containerization       | Docker                            |
| Local Kubernetes       | kind / k3d                        |
| Packaging              | Helm                              |
| API                    | Go HTTP framework to be finalized |

If Go implementation progress becomes a blocking issue, the data collection layer can be ported to the Python Kubernetes client. The priority is delivering a complete, working system.

---

# Status

**Active Development**

This repository currently documents the planned architecture and implementation scope. Features listed as planned or unchecked in the roadmap are not yet implemented.

Measured cost savings and optimization claims will be added only after reproducible experiments have been completed.

---

# License

To be added.

# Author

Built as a cloud and Kubernetes systems project focused on resource utilization, workload right-sizing, observability, and infrastructure cost optimization.
