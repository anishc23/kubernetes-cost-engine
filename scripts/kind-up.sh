#!/usr/bin/env bash
# Create a kind cluster with Prometheus and the demo workloads, ready for the
# end-to-end test.
set -euo pipefail

CLUSTER="${1:-cost-optimizer}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

for tool in kind kubectl docker; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required but not installed"
done
docker info >/dev/null 2>&1 || die "the Docker daemon is not running"

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "cluster '$CLUSTER' already exists; reusing it"
else
  log "creating kind cluster '$CLUSTER'"
  kind create cluster --config "$ROOT/deploy/kind/cluster.yaml" --name "$CLUSTER" --wait 120s
fi

kubectl config use-context "kind-$CLUSTER" >/dev/null

log "deploying Prometheus"
kubectl apply -f "$ROOT/deploy/kind/prometheus.yaml"
kubectl -n monitoring rollout status deploy/prometheus --timeout=180s

log "building the workload generator image"
docker build -q -f "$ROOT/cmd/workload-gen/Dockerfile" \
  -t k8s-cost-optimizer/workload-gen:dev "$ROOT" >/dev/null
kind load docker-image k8s-cost-optimizer/workload-gen:dev --name "$CLUSTER"

log "deploying demo workloads"
kubectl apply -f "$ROOT/examples/workloads/demo-workloads.yaml"
kubectl -n demo wait --for=condition=available --timeout=180s deploy --all

# The optimizer's data-sufficiency gate requires a minimum number of samples, so
# the cluster must accumulate some history before recommendations are meaningful.
# Waiting here rather than inside the test keeps the test's failures about the
# optimizer rather than about timing.
log "waiting 90s for Prometheus to accumulate usage history"
sleep 90

log "verifying that container metrics are being collected"
metrics=$(kubectl -n monitoring exec deploy/prometheus -- \
  wget -qO- 'http://localhost:9090/api/v1/query?query=count(container_cpu_usage_seconds_total{namespace="demo"})' \
  2>/dev/null || echo '')
if ! grep -q '"status":"success"' <<<"$metrics"; then
  die "Prometheus is not returning container metrics; check 'kubectl -n monitoring logs deploy/prometheus'"
fi
log "container metrics are present"

cat <<EOF

Cluster '$CLUSTER' is ready.

  Prometheus:  http://localhost:30090
  Workloads:   kubectl -n demo get pods

Next:
  make kind-deploy    # install the optimizer chart
  make e2e            # run the end-to-end test
  make kind-down      # tear down
EOF
