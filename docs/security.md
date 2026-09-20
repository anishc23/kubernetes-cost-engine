# Security Model

## Threat model

This tool reads cluster metadata and, optionally, modifies workload resource
requests. The assets worth protecting, and the realistic threats to each:

| Asset | Threat | Mitigation |
|---|---|---|
| Workload availability | a bad recommendation applied automatically restarts pods with too little memory | mutation off by default, behind two independent switches; `BLOCKED`/`INSUFFICIENT_DATA` never applied; bounded maximum decrease |
| Cluster metadata | a compromised optimizer reads what it can see | read-only RBAC by default; no access to secrets or configmaps |
| The optimizer process | container escape or lateral movement | non-root, read-only root filesystem, all capabilities dropped, distroless base with no shell |
| Cost data | savings figures inform business decisions | figures are estimates with stated limits, surfaced everywhere they appear |

The most realistic harm this tool can do is **not** a security breach. It is
applying a memory reduction to a production workload that then gets OOMKilled
under load. The controls below are weighted accordingly.

## Mutation requires two independent decisions

```yaml
analysis:
  apply: false        # the engine will not attempt to write
rbac:
  allowApply: false   # the service account has no write verbs
```

Setting either alone does nothing:

- `apply: true` without `allowApply: true` **fails at Helm render time** with an
  explanatory message, not at runtime.
- `allowApply: true` without `apply: true` grants permission the engine never uses.

They are separate so that a single typo, a careless `--set`, or a copied values
file cannot enable cluster mutation.

### What apply mode can and cannot touch

**Can:** `patch`/`update` on Deployments, StatefulSets and DaemonSets — enough to
change resource requests.

**Cannot, by construction:**

- **Limits.** Only requests are patched, ever. A memory limit determines when the
  kernel kills the process, and no usage series justifies changing that
  automatically.
- **`BLOCKED` or `INSUFFICIENT_DATA` decisions.** These are exactly the cases where
  the engine declined to make a claim; applying them would defeat the safety
  machinery entirely.
- **Delete anything.** The `delete` verb is never granted. The optimizer has no
  reason to remove a workload, and withholding the verb means a bug cannot
  escalate into deleting one.
- **Reduce by more than `applyMaxDecreaseFraction`** (default 0.5) in one step. A
  95% cut may be correct; applying it unattended is not a risk the tool should
  take on an operator's behalf.

## RBAC

Default (read-only):

```yaml
- apiGroups: [""]
  resources: [namespaces, pods]          # pods: restart counts and OOMKilled reasons
  verbs: [get, list, watch]
- apiGroups: [apps]
  resources: [deployments, statefulsets, daemonsets, replicasets]
  verbs: [get, list, watch]
- apiGroups: [batch]
  resources: [cronjobs, jobs]
  verbs: [get, list, watch]
```

**Deliberately absent: secrets and configmaps.** The optimizer never needs to read
application data, and a cost-analysis tool with secret access is a
credential-theft target for no benefit. `replicasets` is read only to resolve the
Pod → ReplicaSet → Deployment ownership chain.

These properties are enforced by tests in `test/helm/chart_test.go`, so a template
change that granted secret access or the delete verb would fail the build.

## Container hardening

```yaml
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532
  seccompProfile: { type: RuntimeDefault }
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities: { drop: [ALL] }
```

The image is `gcr.io/distroless/static-debian12:nonroot` — no shell, no package
manager, no libc. There is nothing for an attacker who achieves execution to
pivot with. The binary is statically linked (`CGO_ENABLED=0`).

`runAsUser` is set explicitly rather than inherited from the base image, so a base
image change cannot silently promote the container to root.

The read-only root filesystem requires a writable `/tmp`, provided as an
`emptyDir`.

Compatible with the `restricted` Pod Security Standard.

## Network exposure

| Port | Serves | Exposure |
|---|---|---|
| 8080 | REST API | ClusterIP by default |
| 9090 | `/metrics` | ClusterIP by default |

Two ports rather than one so the API can be exposed to users while metrics stay
internal.

**The API has no authentication.** This is a deliberate scope decision, not an
oversight: it is read-only, exposes no secrets, and is intended to sit behind the
cluster's existing ingress authentication or be reached by port-forward. If you
expose it beyond the cluster, put an authenticating proxy in front of it. Do not
expose it to the internet.

Responses are marked `Cache-Control: no-store`. A cached recommendation is a stale
description of a cluster that has since changed.

## Prometheus authentication

```yaml
prometheus:
  bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
```

The token is read **fresh on every request**, not cached at startup. Kubernetes
rotates projected service account tokens, and a token cached at startup stops
working partway through the pod's life — surfacing as unauthorized errors hours
after a successful start, which is an unpleasant thing to debug.

## Supply chain

- Pinned dependencies with `go.sum` verification.
- `go mod tidy` keeps the dependency set minimal: client-go, the Prometheus client,
  and yaml. No transitive web frameworks.
- Multi-stage build; no build tooling in the runtime image.
- `-trimpath` so build paths do not leak into the binary.

## Operational guidance

**Before enabling apply mode:**

1. Run in recommendation-only mode for at least one full observation window.
2. Read the `BLOCKED` and `INSUFFICIENT_DATA` recommendations first — they are
   where the engine is telling you something it is not confident about.
3. Apply a handful by hand and watch them.
4. Scope with `kubernetes.namespaces` or `kubernetes.labelSelector` before
   enabling cluster-wide.
5. Keep `applyMaxDecreaseFraction` conservative.

**Alert on:**

- `optimizer_last_analysis_timestamp_seconds` staleness — recommendations served
  after a long gap describe a cluster that no longer exists.
- `optimizer_gates_fired_total{gate="oom-protection"}` rising — workloads started
  being OOMKilled.
- `optimizer_gates_fired_total{gate="data-sufficiency"}` rising — usually the
  metrics pipeline broke, not that the workloads changed.

## Reporting a vulnerability

Open a GitHub security advisory rather than a public issue.
