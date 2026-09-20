# Resume Bullets

Three framings of the same work. **Every number is measured** — from
`experiments/results/` or from the code — and nothing is rounded up.

Lines of code and condition counts are verifiable from the repository. Savings
percentages are allocation-based estimates from the experiments, which is stated
wherever they appear.

---

## Research-oriented
*for research internships, MS applications, systems research groups*

> **Kubernetes Cost Optimization Engine** — Go, Python, Kubernetes, Prometheus
>
> - Identified that Kubernetes right-sizing recommendations cannot be evaluated
>   from observed usage, because the measurement is censored by the configuration
>   under test; designed a ground-truth methodology using generative workloads and
>   counterfactual replay against a held-out horizon.
> - Evaluated 7 right-sizing strategies across 10 controlled workload classes, 4
>   safety margins and 4 observation windows — 48,248 scored conditions — finding
>   that an explicit reliability constraint *reverses* the strategy ranking:
>   mean-based policies lead on raw savings (69%) and yield zero savings that meet
>   the constraint.
> - Demonstrated that no percentile below the maximum is universally safe (p95
>   leaves 34% of demanded CPU work unserved on bursty workloads; p99 leaves 8.5%
>   on spiky ones), and that a safety margin cannot compensate for a statistic
>   that structurally excludes the peak (memory p95 plateaus at 90% survival even
>   at a 2.0× margin).
> - Refuted the project's own initial policy with its own experiments and replaced
>   it with an evidence-derived configuration achieving 100% reliability
>   feasibility across all classes at 33.7% median savings.
> - Produced a fully reproducible benchmark with seeded determinism, provenance
>   stamping and a technical report whose every figure regenerates from two
>   commands.

**Shorter (2 lines):**

> Designed a ground-truth evaluation methodology for Kubernetes resource
> right-sizing, addressing the censoring of usage data by the configuration under
> test; ran 48,248 controlled conditions showing that an explicit reliability
> constraint reverses the ranking of percentile strategies, and that the project's
> own default policy left 34% of demanded CPU work unserved on bursty workloads.

---

## Systems / cloud-oriented
*for infrastructure, SRE, platform and cloud engineering roles*

> **Kubernetes Cost Optimization Engine** — Go, Kubernetes, Prometheus, Helm
>
> - Built a production-shaped Kubernetes resource right-sizing engine in Go
>   (~6,500 lines) integrating client-go and the Prometheus HTTP API, with
>   resource-specific CPU and memory policies reflecting their different failure
>   semantics (throttling versus OOMKill).
> - Implemented a layered safety model — recommendation-only by default, mutation
>   behind two independent switches, OOM-aware gating, least-privilege RBAC with no
>   secret access and no delete verb — enforced by Helm chart render tests.
> - Instrumented the system with Prometheus metrics covering discovery,
>   recommendation decisions, safety-gate activations and query health, with a
>   Grafana dashboard surfacing analysis staleness as a first-class alert signal.
> - Packaged with a Helm chart (non-root, read-only filesystem, dropped
>   capabilities, distroless image) and validated end-to-end on a kind cluster
>   running synthetic load generators through the real cAdvisor → Prometheus →
>   optimizer path.
> - Tested at five levels — unit, property-based invariants, fake Kubernetes
>   clientset, chart rendering, and end-to-end — with ~3,500 lines of tests; a
>   runtime invariant assertion caught a real design error during experimentation.

**Shorter (2 lines):**

> Built a Go-based Kubernetes resource right-sizing engine using client-go and
> Prometheus, with resource-specific CPU/memory policies, OOM-aware safety gating,
> least-privilege RBAC and a Helm chart; validated end-to-end on kind and evaluated
> experimentally across 48,248 controlled conditions.

---

## Finance / enterprise engineering-oriented
*for quantitative infrastructure, trading infrastructure, large-scale platform roles*

> **Kubernetes Cost Optimization Engine** — Go, Python, distributed systems
>
> - Designed and built a resource optimisation system that quantifies the
>   trade-off between infrastructure cost reduction and reliability risk, treating
>   it as a constrained optimisation rather than a single objective — because a
>   weighted score would require an indefensible exchange rate between dollars and
>   outages.
> - Ran a controlled study of 48,248 conditions with statistically rigorous
>   aggregation (medians with percentile-bootstrap intervals, Wilson intervals for
>   proportions), deliberately avoiding significance testing where the
>   distributional assumptions would not hold and documenting why.
> - Quantified the cost of reliability precisely: universal safety across all
>   evaluated workload classes costs ~46 percentage points of achievable savings,
>   enabling an explicit rather than implicit risk decision.
> - Engineered for production operation: versioned REST API, structured logging,
>   graceful shutdown, health/readiness semantics that distinguish process liveness
>   from dependency availability, configuration validation that reports all errors
>   at once, and a documented failure-mode analysis covering 15 scenarios.
> - Documented a credible scaling design from 10 to 100,000 workloads, identifying
>   Prometheus query volume rather than compute as the binding constraint, and
>   ordering the mitigations by leverage.

**Shorter (2 lines):**

> Built a Go system quantifying the cost–reliability trade-off in Kubernetes
> resource allocation as a constrained optimisation; ran 48,248 controlled
> experiments with bootstrap-interval analysis, measuring the price of universal
> reliability at ~46 percentage points of achievable savings.

---

## Numbers you can defend

| Claim | Source |
|---|---|
| 48,248 scored conditions | sum of records across `experiments/results/*.csv` |
| ~6,500 lines implementation, ~3,500 tests | `find . -name '*.go' \| xargs wc -l` |
| p95 leaves 34% of demanded CPU work unserved on bursty-cpu | `research/results.md`, Finding 2.3 |
| Memory max × 1.0 survives 47% of conditions | Finding 3.1 |
| Memory p95 plateaus at 90% survival at 2.0× margin | Finding 3.2 |
| Universal safety costs ~46 percentage points | Finding 5.1 |
| Revised default: 100% feasible, 33.7% median savings | `validate_default` experiment |
| 71% savings on the demo cluster | real kind-cluster run, in the README |

**Do not claim:** production savings, superiority over VPA/Goldilocks/Kubecost, a
novel algorithm, or that the findings generalise to real workloads. The study does
not support any of those, and each is contradicted somewhere in the repository —
which an interviewer may well read.
