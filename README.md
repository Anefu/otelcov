# otelcov

Coverage and lineage for OpenTelemetry monitoring stacks.

otelcov replays recorded telemetry through your collectors, backends, rules, alerts and dashboards to show which workloads are actually watched, explain where coverage breaks, and check pull requests before they merge.

> **Status: early implementation.** The first nodes (replay, SDK environment simulation, k8sattributes) run against the test fixtures in [`testdata/thin-slice/`](testdata/thin-slice/); backend, rules, alerts and reports are next. The design and its decisions live in [AGENTS.md](AGENTS.md).

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
- **Lineage that explains the break.** When a path breaks, the report follows it back to the root cause, including things that *didn't* happen. Here is a text rendering of the expected report for the sidecar bug (the reports themselves are JSON; see `testdata/thin-slice/expected/`):

  ```
  vllm-embed-5c8b9d6f7b-q4m9t: not covered (broken at rule inference:vllm_memory_bytes:max)
    rule      dropped: selector needs k8s_container_name="vllm"; the series has no k8s_container_name
    backend   skipped k8s.container.name: listed for promotion, but absent on the input
    collector skipped k8s.container.name: pod has 2 containers (vllm, log-shipper) and no container.id
    hint      set k8s.container.name=vllm in OTEL_RESOURCE_ATTRIBUTES on the vllm container
  ```

- **Honest about limits.** Anything otelcov can't model or attribute is reported as `unverified`, never guessed.

## Scope

The first release targets one concrete stack: OpenTelemetry Collector daemonset agents and a gateway, Mimir and Loki, Prometheus-style rules, Alertmanager and Grafana dashboards. Everything is built as adapters behind generic interfaces, so other backends and deployment patterns can follow.

## Related tools

otelcov builds on ideas from several tools that each cover part of the problem: Bindplane's snapshot recordings, Elastic's OTTL Playground, OpenTelemetry Weaver's live-check, `mimirtool analyze`, `pint` and `promtool`. See [AGENTS.md](AGENTS.md#prior-art) for how they compare.

## Contributing

The project is at the design stage. Read [AGENTS.md](AGENTS.md) first: it holds every design decision so far, the alternatives considered, and the open questions. Issues and discussion are welcome.

## License

[Apache-2.0](LICENSE)
