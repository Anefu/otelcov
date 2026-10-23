# Thin-slice fixtures

Hand-written inputs and expected outputs for the first end-to-end slice of the coverage checker. They reproduce the multi-container `k8s.container.name` bug: a sidecar silently takes a pod out of an alert.

## The path

```
recording ─▶ source/sdk-env ─▶ collector/k8sattributes ─▶ collector/batch ─▶ backend/mimir-otlp
          ─▶ rule/inference:vllm_memory_bytes:max ─▶ alert/VLLMMemoryHigh ─▶ alertmanager/route
```

Two vLLM pods in `inference`: `vllm-chat` (always one container) and `vllm-embed` (the one that gets a sidecar).

## Scenarios

The story: staging runs single-container pods; a PR adds a sidecar and is caught; the corrected PR passes as simulated; after deploy, a new snapshot confirms it.

| Scenario | Snapshot | Config under test | Expected for `vllm-embed` |
|---|---|---|---|
| `01-single-container` | `baseline` | as recorded | covered |
| `02-sidecar-added` | `baseline` | PR adds a sidecar | not covered; root cause in k8sattributes |
| `04-fix-pr-simulated` | `baseline` | PR adds sidecar + `OTEL_RESOURCE_ATTRIBUTES` | covered_simulated |
| `03-sidecar-fixed` | `fixed` (taken after 04 deployed) | as recorded | covered (observed) |

01, 02 and 04 replay the same data with different config and get three different answers: that's `f(data, config)` doing what-ifs.

## Layout

| Path | What it is |
|---|---|
| `recordings/*.otlp.jsonl` | Tap output: one OTLP/JSON export request per line, as the file exporter writes it |
| `recordings/*.meta.yaml` | Snapshot metadata: the pod specs that were running when it was recorded |
| `k8s/pods-*.yaml` | Pods as the API returns them (spec + status) |
| `topology.yaml` | One collector (`collector`, tapped at `metrics/tap`) joined to the Mimir backend; rules read Mimir, Alertmanager reads the rules |
| `collector/collector.yaml` | Staging collector: the real `metrics` pipeline, plus the `metrics/tap` pipeline that produces recordings |
| `backend/mimir-otlp.yaml` | Mimir limits that decide which resource attributes become labels |
| `rules/`, `alertmanager/` | The rule file and routing tree |
| `requirements/vllm.yaml` | What "covered" means for this slice |
| `scenarios/*.yaml` | One run each: data + config (pods + topology) + requirements + expected report |
| `expected/*.report.json` | Golden reports |
| `promtool/inference.rules.test.yaml` | Cross-checks the rule and alert nodes with the real Prometheus engine |

## Report format

- **`signals`**: everything that flows through the graph. Nodes that only change attributes (k8sattributes) keep the signal's id and emit events. Nodes that change its form or aggregate (backend, rule, alert) create new signals with `from` pointing at their inputs. IDs: `s` = recorded OTLP, `p` = stored series, `r` = rule output, `a` = alert.
- **`events`**: structured lineage, `{id, node, signal, attribute?, action, value?, simulated?, reason?, detail?}`. Actions:
  - `attributed`: sdk-env worked out which pod and container sent the signal
  - `associated`: k8sattributes linked the signal to a pod
  - `set` / `kept`: attribute added / already present and left alone
  - `skipped`: the node could have set or used the attribute but didn't (`reason` says why)
  - `derived`: the node produced a new signal from its inputs
  - `dropped`: the input contributed to no output
  - `routed`: an alert reached a receiver
- **Backend outputs**: data series first (`p1`, `p2`, ...), then one `target_info` series per resource, as Mimir's OTLP translator writes it. Its labels are `job` plus every non-identifying resource attribute.
- **`summary`**: per requirement and pod: `status`, `path` (signal ids), and `receiver` or `broken_at`. Status is `covered`, `not_covered`, or `covered_simulated` when the path needs an attribute only sdk-env provided; `simulated_by` then lists those events. Attributes simulated but not needed (like `k8s.pod.uid` in 04) don't count.
- **`diagnosis`**: per gap, the `chain` of events from where the path broke back to the `root_cause`.

"Covered" means *watched*: the pod's series reaches the alert's input, with the threshold ignored. The third promtool test shows this matches real firing when you inject bad values.

Suggested strictness for golden tests: compare `summary` and `diagnosis` exactly; for `events`, check every expected event is present and ignore extras, so adding more lineage later doesn't break tests.

## Check the fixtures with the real tools

These weren't run when the fixtures were drafted (the binaries weren't available). Run them once:

```sh
promtool check rules rules/inference.rules.yaml
(cd promtool && promtool test rules inference.rules.test.yaml)
amtool check-config alertmanager/alertmanager.yaml
amtool config routes test --config.file=alertmanager/alertmanager.yaml \
  --verify.receivers=inference-pager alertname=VLLMMemoryHigh severity=page team=inference
otelcol-contrib validate --config=collector/collector.yaml
```

## Open points (your call)

1. **Tap and pod association.** A replayed file has no client connection, so k8sattributes couldn't link it to a pod. The tap pipeline therefore runs k8sattributes in passthrough mode, which only records `k8s.pod.ip`. Alternative: SDKs set `k8s.pod.uid` via the downward API and the tap stays processor-free.
2. **What-ifs on pod specs.** Pods keep the same name and IP across scenarios. A real PR changes a Deployment template, so the checker must map each recorded pod to its owner and apply the new template.
3. **Mimir setting name.** `otel_metric_suffixes_enabled` wasn't verified against current Mimir docs.
4. **batch processor.** Modeled as pass-through with no events.
5. **Not in the fixtures yet:** sdk-env's fallback attribution, the ambiguous case (should give `unverified`), and a PR that removes an env attribute.

## sdk-env rules (as decided)

- **Attribution:** match the recorded `service.name` to a container's `OTEL_SERVICE_NAME`; otherwise use the only container with `OTEL_*` env vars; otherwise `unverified`. It uses the snapshot's pod specs, since those were running when the data was recorded.
- **Env resolution:** literal values, `valueFrom.fieldRef` (pod fields) and `$(VAR)` references to earlier env vars. Other sources (`configMapKeyRef`, `secretKeyRef`, `envFrom`) give `unverified`.
- **Diff:** undo the snapshot's env attributes, apply the config's. An attribute is only removed if its recorded value matches what the snapshot's env var said, since apps can also set attributes in code.
- **Consistency check:** if the recording disagrees with the snapshot's env vars, the snapshot metadata is wrong; flag it.

## Changes to the golden reports

- **Backend node built** with Prometheus's real OTLP translator: added the `target_info` series it emits (labels derived from the golden data itself, then confirmed against the translator), and dropped the hand-written `metric.why` explanation from backend `derived` details. Event IDs renumbered accordingly.
- **Rule node built** with the real PromQL engine: rule `derived` details now carry the rule expression in Prometheus's canonical form (`expr`), instead of hand-split `selector` and `aggregation` strings.
- **Alert and route nodes built**: alert `derived` details now carry the expression, what was evaluated after the threshold rewrite, and `for`; route matchers are in Alertmanager's own order (sorted by label name); routing events follow all alert events, since each stage logs in turn.
- **Report built**: the golden test now compares whole reports exactly, event IDs included. The diagnosis explanation is now generated from the lineage (it names the rule, the matcher, the missing label, the attribute it comes from, and the root cause's own reason) instead of hand-written prose.
- **Topology runner**: scenarios now name a topology file (`config: {pods, topology}`) instead of each config separately. The collector is named `collector` in `topology.yaml`, so node names (`collector/k8sattributes`) and the golden reports are unchanged.
- **Identifiers quoted in diagnoses** (02): explanations, hints and k8sattributes' reason for not naming a container put identifiers in backticks, which the report page shows as code. Nothing else changed.
