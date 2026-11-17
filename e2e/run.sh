#!/usr/bin/env bash
# End-to-end test on a throwaway kind cluster with the real Argo Rollouts
# controller and Prometheus:
#   1. an update with the same error rate (1%) must be promoted;
#   2. an update with 5x the error rate (5%) must be aborted by the plugin.
# Writes the AnalysisRuns and a summary to reports/e2e/.
# Needs docker, kind, kubectl and go. KEEP_CLUSTER=1 keeps the cluster.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-seqplugin-e2e}
OUT=${OUT:-reports/e2e}
ROLLOUTS_VERSION=v1.10.2
work=$(mktemp -d)
log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
cleanup() {
    local rc=$?
    if ((rc != 0)) && kubectl cluster-info >/dev/null 2>&1; then
        log "FAILED (exit $rc); diagnostics:"
        kubectl get pods -A -o wide || true
        kubectl -n argo-rollouts describe pods | tail -60 || true
        kubectl -n argo-rollouts logs deploy/argo-rollouts --tail 80 || true
        kubectl get rollout,analysisrun -o wide 2>/dev/null || true
    fi
    if [[ "${KEEP_CLUSTER:-0}" != 1 ]]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
    rm -rf "$work"
}
trap cleanup EXIT

log "building plugin and demo app"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$work/rollouts-plugin-metric-sequential" .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$work/demo-app" ./e2e/demo-app
docker build -q -t seqplugin/argo-rollouts:e2e -f - "$work" <<EOF
FROM quay.io/argoproj/argo-rollouts:${ROLLOUTS_VERSION}
COPY --chmod=0755 rollouts-plugin-metric-sequential /plugins/rollouts-plugin-metric-sequential
EOF
docker build -q -t seqplugin/demo-app:e2e -f - "$work" <<'EOF'
FROM scratch
COPY demo-app /demo-app
USER 65532
ENTRYPOINT ["/demo-app"]
EOF

log "creating kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --wait 180s
# Pull on the host and load into the node, so the test does not depend on
# registry access from inside kind (which fails on some WSL setups).
third_party="prom/prometheus:v3.5.0 curlimages/curl:8.11.1"
for img in $third_party; do docker pull -q "$img"; done
# shellcheck disable=SC2086
kind load docker-image --name "$CLUSTER" seqplugin/argo-rollouts:e2e seqplugin/demo-app:e2e $third_party

log "installing Prometheus and Argo Rollouts $ROLLOUTS_VERSION with the plugin"
kubectl apply -f e2e/manifests/prometheus.yaml
kubectl create namespace argo-rollouts
# Server-side apply: the Rollout CRDs are too large for the last-applied annotation.
kubectl apply --server-side -n argo-rollouts -f "https://github.com/argoproj/argo-rollouts/releases/download/${ROLLOUTS_VERSION}/install.yaml"
kubectl apply --server-side --force-conflicts -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: argo-rollouts-config
  namespace: argo-rollouts
data:
  metricProviderPlugins: |-
    - name: "harshit3355/sequential"
      location: "file:///plugins/rollouts-plugin-metric-sequential"
EOF
# The upstream Deployment uses imagePullPolicy Always; the e2e image exists only inside kind.
kubectl -n argo-rollouts patch deploy argo-rollouts --type json -p '[
  {"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": "seqplugin/argo-rollouts:e2e"},
  {"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "IfNotPresent"}]'
kubectl -n argo-rollouts rollout status deploy/argo-rollouts --timeout 300s
kubectl -n monitoring rollout status deploy/prometheus --timeout 300s

phase() { kubectl get rollout demo -o jsonpath='{.status.phase}'; }
stable_hash() { kubectl get rollout demo -o jsonpath='{.status.stableRS}'; }
# wait_for <phase> <timeout-seconds> [previous-stable-hash]: only counts once the
# controller has observed the latest spec; for Healthy, the newest revision must
# also have become stable and differ from the previous stable one.
wait_for() {
    local want=$1 deadline=$((SECONDS + $2)) old=${3:-} gen obs p cur stable
    while ((SECONDS < deadline)); do
        IFS='|' read -r gen obs p cur stable < <(kubectl get rollout demo -o \
            jsonpath='{.metadata.generation}|{.status.observedGeneration}|{.status.phase}|{.status.currentPodHash}|{.status.stableRS}{"\n"}')
        if [[ "$obs" == "$gen" && "$p" == "$want" ]] &&
            [[ "$want" != Healthy || ( "$cur" == "$stable" && "$stable" != "$old" ) ]]; then
            log "rollout is $p (stable $stable)"
            return 0
        fi
        sleep 10
    done
    log "timed out waiting for $want; rollout is $(phase)"
    kubectl get rollout demo -o yaml | tail -40
    return 1
}
set_version() { # set_version <version> <error-rate>
    kubectl patch rollout demo --type json -p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/env\",\"value\":[{\"name\":\"ERROR_RATE\",\"value\":\"$2\"},{\"name\":\"VERSION\",\"value\":\"$1\"}]}]"
}

log "deploying v1 (1% errors) and load"
kubectl apply -f e2e/manifests/app.yaml
wait_for Healthy 300
sleep 30 # let Prometheus collect stable traffic

log "case 1: v2 with the same 1% error rate must be promoted"
v1=$(stable_hash)
set_version v2 0.01
wait_for Healthy 900 "$v1"

log "case 2: v3 with a 5% error rate (5x) must be aborted"
set_version v3 0.05
wait_for Degraded 900

mkdir -p "$OUT"
runs="kubectl get analysisrun --sort-by=.metadata.creationTimestamp"
$runs -o json > "$OUT/analysisruns.json"
kubectl -n argo-rollouts logs deploy/argo-rollouts | grep -iE 'plugin|sequential' | tail -20 > "$OUT/controller-plugin-log.txt" || true

{
    echo "# End-to-end result"
    echo
    echo "kind cluster, Argo Rollouts $ROLLOUTS_VERSION, Prometheus v3.5.0, demo app behind a Service with two curl load generators."
    echo "Case order: v2 (same 1% errors), then v3 (5% errors). Generated by \`e2e/run.sh\`; raw objects in \`analysisruns.json\`."
    echo
    echo "| AnalysisRun | Phase | Last e-value (reject at 20) | Last window | Message |"
    echo "|---|---|---|---|---|"
    $runs -o jsonpath='{range .items[*]}| {.metadata.name} | {.status.phase} | {.status.metricResults[0].measurements[-1:].metadata.eValue} | {.status.metricResults[0].measurements[-1:].metadata.windowCounts} | {.status.message} |{"\n"}{end}'
    echo
    echo "Every measurement (e-value, window counts):"
    echo
    $runs -o jsonpath='{range .items[*]}- {.metadata.name}:{range .status.metricResults[0].measurements[*]} {.metadata.eValue} ({.metadata.windowCounts});{end}{"\n"}{end}'
} > "$OUT/summary.md"
cat "$OUT/summary.md"

phases=$($runs -o jsonpath='{range .items[*]}{.status.phase},{end}')
if [[ "$phases" != "Successful,Failed," ]]; then
    log "FAIL: expected AnalysisRun phases Successful,Failed, got $phases"
    exit 1
fi
log "PASS"
