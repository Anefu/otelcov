# Agent + gateway fixtures

Hand-written inputs and golden reports for the owner's topology: OpenTelemetry Collector **daemonset agents** (tapped) forwarding to a **gateway**, which writes to Mimir. Every processor runs as real collector-contrib code (0.135), except `k8sattributes` and `resourcedetection`, which are custom nodes, and `batch` / `memory_limiter`, which pass data through unchanged.

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

All three replay the same recording with different config.

| Scenario | Config under test | Expected for both pods |
|---|---|---|
| `01-baseline` | as deployed | covered |
| `02-team-condition-pr` | PR changes the team statement's condition to namespace `inference-prod` | not covered: the diagnosis names the statement, says its condition is false, and what it would have set |
| `03-bad-join` | agent sends gRPC to the gateway's HTTP port (4318) | unverified: validation can't trust the agent → gateway join; the topology problem is in the report |

## Layout

| Path | What it is |
|---|---|
| `topology*.yaml` | The thin topology files (decision 25): which config is which collector, the backend, consumers, joins, `external` exporters, config hashes |
| `collector/agent.yaml` | Daemonset agent: `metrics` pipeline plus the `metrics/tap` pipeline that records input (k8sattributes in passthrough mode, file exporter) |
| `collector/gateway.yaml` | Gateway deployment |
| `collector/gateway-pr.yaml`, `collector/agent-badport.yaml` | The changed configs for scenarios 02 and 03 |
| `recordings/staging.otlp.jsonl` | Tap output: one OTLP/JSON export request per line |
| `recordings/staging.meta.yaml` | Snapshot metadata: pod specs and what each resourcedetection processor detected, per node, with its config at recording time |
| `k8s/pods.yaml` | Pods as the API returns them |
| `backend/mimir.yaml` | Mimir limits: promoted resource attributes (including `team`), suffixes on |
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
sha256sum collector/*.yaml backend/mimir.yaml
```

and update the `sha256` fields, or the report will warn that the topology may be stale.

## Changes to the golden reports

- **Created** together with the topology runner, real processor nodes and OTTL statement explanations. Reviewed by hand: 01 covered for both pods; 02 not covered for both, root cause the skipped `transform/team` statement, then Mimir's `skipped` promotion of `team`, then the rule's `dropped`; 03 unverified for both via the join event.
- **Report section renamed**: `topology` is now `problems` (03), since it also carries pod fields otelcov doesn't know. Nothing else changed.
