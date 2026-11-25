# Agent + gateway fixtures

Hand-written inputs and golden reports for the owner's topology: OpenTelemetry Collector **daemonset agents** (tapped) forwarding to a **gateway**, which writes to Mimir. Every processor runs as real collector-contrib code (0.139, which behaves like 0.134 here), except `k8sattributes` and `resourcedetection`, which are custom nodes, and `batch` / `memory_limiter`, which pass data through unchanged.

The data and configs are synthetic, shaped like a small kind cluster (`kind-worker`, `kind-worker2`), so the same scenarios can be re-run later against a real recording from a kind cluster.

## The path

```
recording ─▶ source/sdk-env
  agent:   k8sattributes ─▶ resourcedetection ─▶ resource ─▶ batch ─▶ otlp/gateway
  gateway: memory_limiter ─▶ filter/drop-debug ─▶ transform/team ─▶ deltatocumulative ─▶ batch
             ├─▶ otlphttp/mimir ─▶ backend/mimir ─▶ rule/team:vllm_memory_bytes:max ─▶ alert/VLLMMemoryHigh ─▶ alertmanager/route
             └─▶ debug (external: leaves the modeled system; gets copies c1, c2, ...)
```

Two vLLM pods in `inference`:

- `vllm-chat` on `kind-worker`: one container. It sends `process.memory.usage`, a `debug.heartbeat` the gateway drops, and a delta `vllm.requests` that deltatocumulative converts.
- `vllm-embed` on `kind-worker2`: a `log-shipper` sidecar, so its SDK sets `k8s.container.name=vllm` itself (the motivating bug's fix).

The rule selects `team="inference"`, and `team` comes from the gateway's `transform/team` statement, so ownership depends on a collector config rather than on the rule.

## Scenarios

01–06 and 09 replay the same recording (`staging`, one file for both agents, without nodes) with different config; 07 replays it split per agent, and 08 a recording where both apps reached one agent.

| Scenario | Config under test | Expected for both pods |
|---|---|---|
| `01-baseline` | as deployed | covered |
| `02-team-condition-pr` | PR changes the team statement's condition to namespace `inference-prod` | not covered: the diagnosis names the statement, says its condition is false, and what it would have set |
| `03-bad-join` | agent sends gRPC to the gateway's HTTP port (4318) | unverified: validation can't trust the agent → gateway join; the topology problem is in the report |
| `04-init-container-pr` | PR adds an `istio-init` init container to vllm-chat | vllm-chat not covered: k8sattributes counts init containers, so it can't name the container (root cause in the diagnosis); vllm-embed still covered |
| `05-suffixes-off-pr` | PR turns off Mimir's `otel_metric_suffixes_enabled` | not covered: the series arrive as `process_memory_usage`, the rule selects `process_memory_usage_bytes`; root cause Mimir's translation |
| `06-team-from-namespace-pr` | the agent copies `team` from the namespace's `team` label, and the gateway runs 02's PR | covered: `team` comes from the namespace (`agent/k8sattributes`, "from namespace `inference`'s label `team`"), so the statement no longer matters |
| `07-node-filter` | each agent only watches pods on its own node (`filter.node_from_env_var: K8S_NODE_NAME`, `{fieldRef: spec.nodeName}` in the topology); one recording per agent | covered: each app's data reached its own node's agent |
| `08-node-filter-via-service` | the same agents; recorded while both apps sent to the agents' Service, so both reached the agent on `kind-worker2` | vllm-chat not covered: the agent on `kind-worker2` never sees it (it runs on `kind-worker`), so no pod attributes; root cause `agent/k8sattributes`, hint to send to its own node's agent. vllm-embed covered |
| `09-node-filter-unknown-node` | the same agents, replayed from `staging`, which doesn't say which agent received each line | unverified: whether the agent sees each pod depends on its node, which isn't known |

## Layout

| Path | What it is |
|---|---|
| `topology*.yaml` | The thin topology files (decision 25): which config is which collector, the backend, consumers, joins, `external` exporters, config hashes |
| `collector/agent.yaml` | Daemonset agent: `metrics` pipeline plus the `metrics/tap` pipeline that records input (k8sattributes in passthrough mode, file exporter) |
| `collector/gateway.yaml` | Gateway deployment |
| `collector/gateway-pr.yaml`, `collector/agent-badport.yaml` | The changed configs for scenarios 02 and 03 |
| `recordings/staging.otlp.jsonl` | Tap output: one OTLP/JSON export request per line, both agents in one file |
| `recordings/kind-worker*.otlp.jsonl`, `recordings/staging-nodes.meta.yaml` | The same lines split per agent, and a snapshot listing each with its node (scenario 07) |
| `recordings/via-service.*` | Both lines as recorded by the agent on `kind-worker2` (scenario 08) |
| `collector/agent-node-filter.yaml`, `topology-node-filter.yaml` | The agent with the node filter, and a topology giving each agent instance its node (scenarios 07–09) |
| `recordings/staging.meta.yaml` | Snapshot metadata: the recording, pod specs and what each resourcedetection processor detected, per node, with its config at recording time |
| `k8s/pods.yaml` | Pods as the API returns them |
| `k8s/pods-init-container.yaml` | The same pods, with an init container added to vllm-chat (scenario 04) |
| `k8s/cluster-namespace-team.yaml` | The same pods and their namespace, labeled `team: inference`, as one List (scenario 06) |
| `collector/agent-namespace-team.yaml`, `topology-namespace-team.yaml` | The agent with a namespace label rule, and the topology pairing it with `gateway-pr.yaml` (scenario 06) |
| `backend/mimir.yaml` | Mimir limits: promoted resource attributes (including `team`), suffixes on |
| `backend/mimir-suffixes-off.yaml`, `topology-suffixes-off.yaml` | The same with suffixes off, for scenario 05 |
| `rules/`, `alertmanager/` | The rule file and routing tree |
| `requirements/vllm.yaml` | What "covered" means here |
| `scenarios/*.yaml`, `expected/*.report.json` | Runs and their golden reports |

## Things these fixtures exercise

- **Topology runner**: data enters at the agent's tapped receivers and follows joins to the gateway and Mimir. A pipeline that exports to two places makes copies with new IDs (`c1`, `c2`, ...; node `topology`, action `derived`); `debug` is listed as `external`, so its copies end with `exported`.
- **Real processors with lineage**: `filter` names the condition that dropped an item; `transform` names the statement that set an attribute, or, when its condition is false, the statement that didn't run and what it would have set (`skipped`, detail `would_set`).
- **resourcedetection from the snapshot**: values per Kubernetes node, `override: false` respected.
- **Validation**: the port check in 03. Hashes in the topology files match the configs; change a config without updating its hash and the report gets a warning.

## Regenerating hashes

After editing a collector, backend or Alertmanager config on purpose:

```sh
sha256sum collector/*.yaml backend/*.yaml
```

and update the `sha256` fields, or the report will warn that the topology may be stale.

## Changes to the golden reports

- **Created** together with the topology runner, real processor nodes and OTTL statement explanations. Reviewed by hand: 01 covered for both pods; 02 not covered for both, root cause the skipped `transform/team` statement, then Mimir's `skipped` promotion of `team`, then the rule's `dropped`; 03 unverified for both via the join event.
- **Report section renamed**: `topology` is now `problems` (03), since it also carries pod fields otelcov doesn't know. Nothing else changed.
- **04 created** (2026-10-28) with the k8sattributes parity fix: vllm-chat not covered, root cause `agent/k8sattributes` ("pod has 2 containers (vllm, istio-init (init container))…"), vllm-embed covered. The other reports didn't change.
- **Identifiers quoted, one hint for a skipped statement** (02): explanations put identifiers in backticks; the two hints became one that says what to change ("Make the condition `...` in `gateway/transform/team` hold for this pod, or set `team` another way") instead of restating the explanation; the skipped events carry the statement's condition as `detail.where`.
- **05 created** (2026-10-28) with name-variant near misses (decision 60): both pods not covered at the rule, a `dropped` event on `__name__` ("the selector wants metric \"process_memory_usage_bytes\"; this series is named \"process_memory_usage\", …"), root cause Mimir's `derived` event for the series. Before decision 60 this run gave `unverified` with no diagnosis. The other reports didn't change.
- **06 created** (2026-10-28) with k8sattributes namespace labels (decision 61): both pods covered, `team` set by `agent/k8sattributes` from the namespace's label; the PR's transform statement leaves no `skipped` event, since running it without its condition changes nothing. The other reports didn't change: the fixtures' k8sattributes configs list only attributes that were already modeled.
- **Recordings move into the snapshot metadata** (decision 64): scenarios name only the snapshot; `staging.meta.yaml` lists `staging.otlp.jsonl`. No report changed.
- **07–09 created** (2026-11-24) with the k8sattributes `filter` (decisions 64, 65). 07: both covered; its report differs from 01 only in signal origins and event order (each agent's recording runs to the end before the next). 08: vllm-chat not covered at the rule (no `k8s_container_name`), root cause `agent/k8sattributes`'s `skipped` `k8s.container.name` ("pod `inference/vllm-chat-…` matches …, but the processor never sees it: it runs on node `kind-worker`, …"), hint to send to the collector on `kind-worker`; resourcedetection sets `host.name: kind-worker2`, the agent's own host. 09: both unverified, by an `unverified` event at `agent/k8sattributes` per pod.
