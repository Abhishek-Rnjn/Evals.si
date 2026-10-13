#!/usr/bin/env bash
# The agent harness's deploy step, played for the research assistant (the Deep
# Agents service in the demo chart): a release goes live only if it passes its
# deploy gate.
#
#   examples/demo/deploy/deploy.sh VERSION [MODEL]
#
# 1. Stage: the release runs as <release>-deepagents-candidate.
# 2. Gate: ../research-assistant-gate.yaml is applied as the EvalRun
#    research-assistant-gate-<version>, which calls the candidate over HTTP on
#    its test set.
# 3. Promote when the run Succeeds: production (<release>-deepagents) moves to the
#    release, with its version on every trace. When a gate fails, production
#    stays as it was and this exits 3, as `evalsi run` does.
#
# MODEL is the model the release calls (default: the chart's agents.model.name);
# with the demo's mock, mock-regressed plays a broken release. One version is
# one gate run: running a version again reads the result of its first run.
#
#   NAMESPACE (evalsi)  RELEASE (evalsi-demo)  CHART (this directory's parent)
#   GATE_TIMEOUT        seconds to wait for the gate run (900)
set -euo pipefail

version="${1:?usage: deploy.sh VERSION [MODEL]}"
model="${2:-}"
ns="${NAMESPACE:-evalsi}"
release="${RELEASE:-evalsi-demo}"
here="$(cd "$(dirname "$0")" && pwd)"
chart="${CHART:-$here/..}"
timeout="${GATE_TIMEOUT:-900}"
run="research-assistant-gate-$(printf '%s' "$version" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9-' '-')"

upgrade() {
  helm upgrade "$release" "$chart" -n "$ns" --reuse-values --wait --timeout 10m "$@" >/dev/null
}
current() {
  kubectl get deployment "$release-deepagents" -n "$ns" -o jsonpath='{.metadata.annotations.evals\.si/version}'
}

echo "staging $version (production is at $(current))"
upgrade --set deepagents.candidate.enabled=true \
  --set-string deepagents.candidate.version="$version" --set-string deepagents.candidate.model="$model"

echo "gating $version: EvalRun $run"
sed "s/^  name: research-assistant-gate\$/  name: $run/" "$chart/research-assistant-gate.yaml" | kubectl apply -n "$ns" -f - >/dev/null
deadline=$((SECONDS + timeout))
while :; do
  phase="$(kubectl get evalrun "$run" -n "$ns" -o jsonpath='{.status.phase}')"
  case "$phase" in Succeeded | Failed | Error | Cancelled) break ;; esac
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "the gate run $run did not finish in ${timeout}s (phase ${phase:-none})" >&2
    exit 1
  fi
  sleep 5
done
kubectl get evalrun "$run" -n "$ns"

if [ "$phase" != Succeeded ]; then
  upgrade --set deepagents.candidate.enabled=false
  if [ "$phase" = Failed ]; then
    echo "blocked: $version failed its gate; production stays at $(current)" >&2
    exit 3
  fi
  echo "blocked: the gate run for $version ended $phase; production stays at $(current)" >&2
  exit 1
fi

upgrade --set-string deepagents.version="$version" --set-string deepagents.model="$model" \
  --set deepagents.candidate.enabled=false
echo "deployed $version: production is at $(current)"
