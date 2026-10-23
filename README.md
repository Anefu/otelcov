# otelcov

Coverage and lineage for OpenTelemetry monitoring stacks.

otelcov replays recorded telemetry through your collectors, backends, rules, alerts and dashboards to show which workloads are actually watched, explain where coverage breaks, and check pull requests before they merge.

> **Status: early implementation.** Metrics work end to end, from recorded OTLP through daemonset agents and a gateway (real collector-contrib processors), Mimir, recording rules, alerts and Alertmanager routing, against the test fixtures in [`testdata/`](testdata/). It hasn't been run against a real cluster yet. The design and its decisions live in [AGENTS.md](AGENTS.md).

## The problem

Monitoring breaks silently, often in a layer nobody touched.

A real example: an alert watched vLLM pods through a recording rule that selects `k8s_container_name="vllm"`. Someone added a log-shipper sidecar to one of the pods. The OpenTelemetry `k8sattributes` processor can only name the container when a pod has exactly one, so that pod's metrics lost their container label. The rule stopped matching them, and the alert stopped watching the pod. Nobody edited the alert or the rule, nothing failed, and nothing said so.

Existing tools each check one layer: collector configs, rule syntax, whether metrics exist. None follows a single workload's telemetry through the whole stack.

## How it works

otelcov models the monitoring stack as one graph:

```
apps → collectors (agent, gateway) → backends (Mimir, Loki) → recording rules → alerts → Alertmanager routes → receivers
                                                          └→ dashboards
```

- **Recorded data, real code.** A tap in your staging collectors records the telemetry they receive. otelcov replays it through the graph, running the real Prometheus engine, Alertmanager routing and collector processor code wherever it can.
- **Every node is `f(data, config)`.** To check a pull request, keep the recorded data and swap in the PR's config: pod specs, collector config, rules, routes.
- **Coverage, per workload.** You state requirements in OpenTelemetry terms, e.g. "every vLLM pod's memory must reach `VLLMMemoryHigh`, which pages the inference team". otelcov reports, for each pod, whether a path exists.
- **Lineage that explains the break.** When a path breaks, the report follows it back to the root cause, including things that *didn't* happen (see Usage).
- **Honest about limits.** Anything otelcov can't model or attribute is reported as `unverified`, never guessed.

## Usage

otelcov runs *scenarios*: a recording, the config under test (pods, and a topology file naming the collector, Mimir, rules and Alertmanager configs and how they connect) and coverage requirements.

```console
$ go run ./cmd/otelcov scenario testdata/thin-slice/scenarios/02-sidecar-added.yaml
02-sidecar-added (evaluated at 2026-10-23T10:00:30Z)
  covered              inference/vllm-chat-6f9d8b7c4d-k2x7p  [vllm-memory-watched]
      path: s1 → p1 → r1 → a1 → inference-pager
  NOT COVERED          inference/vllm-embed-5c8b9d6f7b-q4m9t  [vllm-memory-watched]
      path: s2 → p2 ✗ rule/inference:vllm_memory_bytes:max
      why: rule inference:vllm_memory_bytes:max only selects series matching k8s_container_name="vllm", and this pod's series has no k8s_container_name label, so it never reaches VLLMMemoryHigh. k8s_container_name comes from resource attribute k8s.container.name, which collector/k8sattributes didn't set: pod has 2 containers (vllm, log-shipper) and the resource has neither container.id nor k8s.container.name, so the container can't be identified.
      fix: Set k8s.container.name=vllm in OTEL_RESOURCE_ATTRIBUTES on the vllm container.
      fix: Or have the SDK send container.id, so k8sattributes can match the container.
```

Processors explain themselves too. Here a pull request changes the condition of a gateway `transform` statement, and the rule's `team="inference"` matcher loses both pods:

```console
$ go run ./cmd/otelcov scenario testdata/agent-gateway/scenarios/02-team-condition-pr.yaml
02-team-condition-pr (evaluated at 2026-10-23T10:00:30Z)
  NOT COVERED          inference/vllm-chat-6f9d8b7c4d-k2x7p  [vllm-memory-watched]
      path: s1 → p1 ✗ rule/team:vllm_memory_bytes:max
      why: rule team:vllm_memory_bytes:max only selects series matching team="inference", and this pod's series has no team label, so it never reaches VLLMMemoryHigh. team comes from resource attribute team, which gateway/transform/team didn't set: resource statement `set(attributes["team"], "inference") where attributes["k8s.namespace.name"] == "inference-prod"` didn't run: its condition is false for this signal.
      fix: Statement `set(attributes["team"], "inference") where attributes["k8s.namespace.name"] == "inference-prod"` in gateway/transform/team would set team=inference, but its condition is false for this pod.
      fix: Fix the condition, or set team another way.
  ...
```

The topology file is checked on every run: an exporter sending to the wrong port, a receiver listening on localhost only, or an exporter that isn't connected to anything is reported, and coverage through it is marked `unverified` or the run stops.

`-json` prints the full report, with every signal and lineage event. `-html report.html` writes one self-contained page for all the scenarios: a coverage matrix, and for any pod its route through the stack, with what each node did to its signal and where the route breaks. The exit status is 1 when anything is not covered, so it can gate CI.

## Try it on a cluster

[`dev/kind/`](dev/kind/) runs daemonset agents, a gateway, Mimir and Alertmanager on a local kind cluster with Tilt. You capture what the agents record, run otelcov on it, and compare its answers with what Mimir and Alertmanager really did, including for a pull request you then deploy.

## Scope

The first release targets one concrete stack: OpenTelemetry Collector daemonset agents and a gateway, Mimir and Loki, Prometheus-style rules, Alertmanager and Grafana dashboards. Everything is built as adapters behind generic interfaces, so other backends and deployment patterns can follow.

## Related tools

otelcov builds on ideas from several tools that each cover part of the problem: Bindplane's snapshot recordings, Elastic's OTTL Playground, OpenTelemetry Weaver's live-check, `mimirtool analyze`, `pint` and `promtool`. See [AGENTS.md](AGENTS.md#prior-art) for how they compare.

## Contributing

The project is at an early stage. Read [AGENTS.md](AGENTS.md) first: it holds every design decision so far, the alternatives considered, and the open questions. Issues and discussion are welcome.

## License

[Apache-2.0](LICENSE)
