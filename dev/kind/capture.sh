#!/usr/bin/env bash
# Copies the agents' recordings off the kind cluster and builds a snapshot
# otelcov can replay, in captured/:
#   tap/<node>.jsonl, detected/<node>.jsonl   raw copies from each agent
#   pods.yaml, agent.yaml                      the pods and the agent config that ran
#   recording.otlp.jsonl, snapshot.meta.yaml   what otelcov replays
#   scenarios/*.yaml                           otelcov/scenarios with eval_time set
# Each run replaces the previous capture.
set -euo pipefail
cd "$(dirname "$0")"

CONTEXT=kind-otelcov
k() { kubectl --context "$CONTEXT" "$@"; }

out=captured
rm -rf "$out"
mkdir -p "$out/tap" "$out/detected"

agents=$(k -n observability get pods -l app.kubernetes.io/name=otel-agent \
  --field-selector=status.phase=Running \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}')
if [ -z "$agents" ]; then
  echo "no running agent pods in kind-otelcov" >&2
  exit 1
fi
while read -r pod node; do
  [ -n "$pod" ] || continue
  k -n observability exec "$pod" -c tap-reader -- cat /tap/metrics.otlp.jsonl > "$out/tap/$node.jsonl"
  k -n observability exec "$pod" -c tap-reader -- cat /tap/detected.otlp.jsonl > "$out/detected/$node.jsonl"
done <<< "$agents"

# What ran: the pods (snapshot and config under test alike), and the agent
# config, which may be a --agent variant.
k -n inference get pods -o yaml > "$out/pods.yaml"
k -n observability get configmap otel-agent-config -o jsonpath='{.data.agent\.yaml}' > "$out/agent.yaml"

bin/snapshot -dir "$out" -agent-config "$out/agent.yaml" -templates otelcov/scenarios
echo "Captured into dev/kind/$out. Run the otelcov resource next."
