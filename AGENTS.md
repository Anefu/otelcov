# AGENTS.md

Guidance for coding agents (and humans) working in this repository. Read it fully before changing anything.

## What this project is

A verification tool for OpenTelemetry-based monitoring stacks. It models the whole stack as one dataflow graph:

```
apps → collectors (agent, gateway) → backends (Mimir, Loki) → recording rules → alerts → Alertmanager routes → receivers
                                                          └→ dashboards
```

It replays recorded telemetry through that graph and reports, for each workload, whether its signals reach the alerts and dashboards meant to watch it. When a path breaks, it explains where and why, with structured lineage. It also answers what-ifs: "if this PR merges, which workloads lose coverage?"

The goal is a meaningful open-source contribution, so the design is generic. The first release targets one concrete stack (see Scope).

### Motivating bug

A recording rule used by an alert silently stopped covering some pods. The OTel `k8sattributes` processor only sets `k8s.container.name` when a pod has a single container, or when the telemetry already carries `container.id` or `k8s.container.name`. Adding a sidecar removed the container label, the rule's `k8s_container_name="vllm"` matcher stopped matching, and the alert stopped watching that pod. Nobody touched the alert or the rule. The fixtures in `testdata/thin-slice/` reproduce exactly this.

### Gap it fills

As far as we could find, no existing tool follows one workload's signal across collectors, backend, rules, alerts and routes. Existing tools each cover one step (see Prior art).

## Working agreements

- **The project owner makes the important design decisions.** Present options with their trade-offs and let the owner choose. Do not pick silently, even when one option seems obvious. Record every decision in the Decision log below, with the alternatives considered.
- **Prefer reusing real upstream code over re-implementing it**: Prometheus's PromQL engine and parser, Alertmanager routing, collector-contrib processors, OTTL. Hand-written models drift from reality.
- **Never guess.** Anything the tool can't model or attribute is reported as `unverified`, with the reason. A wrong "covered" is worse than an honest "unverified".
- **Keep the `f(data, config)` split** (see Architecture). Nodes must not read config from anywhere except their config input.
- **Golden files are specs.** When a fixture changes, update the expected reports deliberately and explain why. Never edit an expected report just to make a test pass.
- **No real telemetry in the repo.** Recordings in `testdata/` are synthetic.
- **Clean room for AGPL code.** Never copy code, grammar files (e.g. Loki's goyacc grammar) or tests from AGPL projects (Loki, Mimir, Grafana, Tempo). Learn LogQL from its documentation. Talking to a real Loki over its API in tests is fine; importing its packages is not.

## Status

**Agent + gateway topologies work end to end**: replay, sdk-env, then the collectors as the topology file connects them (real collector-contrib processors, custom k8sattributes and resourcedetection), Mimir backend, recording rules, alerting rules, Alertmanager routing and the report. Transform and filter explain themselves per OTTL statement and condition. `otelcov scenario` runs scenarios from the command line. All seven scenarios match their golden reports exactly. Repository: `github.com/Anefu/otelcov`, Apache-2.0.

- `testdata/thin-slice/`: the first end-to-end slice, one collector (4 scenarios). See its README.
- `testdata/agent-gateway/`: daemonset agents + gateway, the owner's topology (3 scenarios). See its README.
- `dev/kind/`: a kind cluster running the owner's topology for real (Tilt), with capture, otelcov and a ground-truth comparison against Mimir and Alertmanager. See its README.
- **First real run** (2026-10-23, kind on colima, Kubernetes 1.37): otelcov's answers on a real recording matched the real stack.
  - Baseline: both pods covered, and compare agreed.
  - Gateway team-condition PR, predicted from the old recording and then deployed: both pods not covered. The real stack broke at the same place: series reach Mimir, but the rule output disappears. compare agreed.
  - Apps reached their node's agent with their own pod IP, and the resourcedetection capture gave real per-node values.
  - **The motivating bug, for real**: the apps what-if that drops `OTEL_RESOURCE_ATTRIBUTES` from vllm-embed (the pod with a sidecar), deployed and captured. otelcov: vllm-embed not covered, broken at the recording rule, root cause `agent/k8sattributes` (2 containers, and neither `container.id` nor `k8s.container.name` on the resource); vllm-chat still covered. The real stack: vllm-embed's series reach Mimir without `k8s_container_name`, no rule output, no alert. compare agreed for both pods, before (01) and after (04). So the real k8sattributes (collector 0.135.0 at the time) behaves as otelcov's custom node says.
  - Earlier, in a local rehearsal with the real binaries, otelcov's predicted series labels matched Mimir 3.2.3's exactly, `target_info` included.
- The `testdata/` fixtures pass the real tools: `promtool` 3.5.0 (`check rules`, and the thin slice's rule unit test), `amtool` 0.28.1 (`check-config`, both routes) and `otelcol-contrib validate` 0.134.1.

## Scope of the first release

The owner's stack only, behind generic interfaces:

- OpenTelemetry Collector as **daemonset agents + a gateway**, OTel-native pipelines: OTLP from apps, plus ServiceMonitors / scrape targets scraped by the collectors themselves
- **Mimir** (OTLP ingestion) and **Loki**
- Prometheus-style recording and alerting rules, **Alertmanager**
- **Grafana** dashboards

Other deployment patterns and backends come later, as adapters.

## Architecture (decided)

### The graph

- Every component is a node; edges mean "feeds into". Mostly a DAG.
- Each node is `f(data, config)`:
  - **data**: the live telemetry flowing through
  - **config**: everything static (pod specs, collector config, rules, routes, dashboards)
- **What-ifs**: keep the recorded data, swap in a PR's config, re-run.
- Nodes take and return **sets** of signals, not single items, so aggregations, Alertmanager grouping and inhibition fit.
- Draft core interface:

  ```go
  type Node interface {
      Name() string
      Run(in Signals, cfg Config) (Signals, error)
  }
  ```

- Nodes that only change attributes keep a signal's ID and emit events. Nodes that change its form or aggregate (backend, rule, alert) create new signals with `from` links.

### Where the graph starts: the tap

- The graph starts at the **collector's input**, not at the backend. Mimir and Loki only hold data after the collector has already run, which would make collector what-ifs impossible.
- Source data is captured by a **tap inside the collector**: an extra pipeline per receiver that writes raw input to disk with the **file exporter** (OTLP/JSON, one export request per line, with rotation).
- The tap is planned for the **daemonset agents**: apps send to the agent on their own node, so the connection IP is the pod's own; host-only receivers (kubeletstats, filelog, hostmetrics) only run there. Cost: one recording file per node to collect.
- Tapping and checks run **only in staging/test**, never in prod.
- **Raw recordings** (not deduplicated label-set inventories), so real values and timestamps are kept and stepped time stays possible.
- **Log bodies are masked by pattern** in the tap, with OTTL `replace_pattern`. Guidance (proposed, not separately decided): mask values, not structure (keep quotes, keys, delimiters so `| json` and `| logfmt` still parse); replace with placeholders or hashes, not nothing; mask attribute values such as URLs and headers too; scanning recordings for leaked secrets can itself be a check.
- **Snapshots** store the config they were recorded with (currently: pod specs, see `recordings/*.meta.yaml`), because the recorded data already contains its effects.

### Where checks run

- **Continuously in staging**, next to the collector.
- **In CI for PRs**: replay the latest snapshot with the PR's config.
- Rule inputs that never appear in staging (e.g. error counters that only exist after an error) are marked **`unverified` and reported**, not failed.

### Nodes

- **Processor nodes (the "middle option")**:
  - Run the **real collector-contrib processor code** in-process, with a wrapper that compares input and output to emit `set` / `removed` / `dropped` events.
  - For **transform and filter**, also emit statement-level events via the OTTL library (which statement fired, which condition was false).
  - **k8sattributes is a custom node**: its decisions depend on pod data, and its key behavior is a non-event (not setting an attribute) that a comparison can't see.
  - Processor types without a node: data passes through, everything downstream is `unverified`.
  - Build against the **same collector-contrib version the deployed collectors run**, or real-code nodes can disagree with production.
- **sdk-env node** (first node after the recording): simulates `OTEL_RESOURCE_ATTRIBUTES` changes, which happen in the app *before* the tap and are therefore baked into recordings.
  - Attribution: match the recorded `service.name` to a container's `OTEL_SERVICE_NAME`; otherwise the only container with `OTEL_*` env vars; otherwise `unverified`. Uses the snapshot's pod specs.
  - Env resolution: literal values, `valueFrom.fieldRef` (pod fields) and `$(VAR)` references. `configMapKeyRef`, `secretKeyRef` and `envFrom` give `unverified`.
  - Diff: undo the snapshot's env attributes, apply the config's. Only remove an attribute if its recorded value matches what the snapshot's env var said (apps can also set attributes in code).
  - If the recording disagrees with the snapshot's env vars, the snapshot metadata is wrong: flag it.
  - Results that depend on a simulated attribute get status `covered_simulated`; the live staging check confirms them after deploy.
- **Backend node**: models OTLP → stored data (Mimir: attribute promotion, name translation; Loki: stream labels vs structured metadata).
- **Rule node**: the **real PromQL engine** decides which outputs exist; an **AST explainer** built on Prometheus's `promql/parser` package explains label flow (`sum by` keeps, `without` drops, `rate()` drops the name, `group_left` copies, `label_replace` creates). Matchers are real `labels.Matcher` objects. If explainer and engine disagree, that's an explainer bug and gets flagged. AST node types the explainer doesn't handle yet give `unverified`.
- **Alert node**: evaluates the alert expression's input; threshold handling, see Coverage semantics.
- **Route node**: real Alertmanager routing code.
- **Dashboard nodes**: each panel query is a leaf consumer node, like a rule without outputs. Template variables use **explicit value lists in the requirements** for now; the checker runs each panel once per combination. A panel using an unlisted variable is `unverified`.

### Checks

- **Forward (coverage)**: for each workload and failure mode, is there a complete path to a human?
- **Backward (dead consumers)**: does every alert and dashboard panel trace back to series something actually produces?
- Checks are **Go code**, one interface (take the graph, return findings), with shared graph helpers ("paths from this workload", "consumers of this series") so each check stays small.

### Requirements

- Proposed principle (the owner hasn't explicitly confirmed it): written **in OTel terms, not backend terms**: "every pod with `app=vllm`: `process.memory.usage` must reach alert X, then receiver Y". The same requirement then works whatever backend stores the data. Draft format: `testdata/thin-slice/requirements/vllm.yaml`.

### Topology

- Each collector's internals (pipelines, processors, order) come from **its own config**. A separate **thin topology file** holds only what configs can't say: which config is which component, and how components connect ("joins"): exporter → receiver, exporter → backend, dashboard datasource UID → backend.
- The topology file is written by an **agent skill** (discovery) and committed and reviewed like code. It must stay hand-writable for people without an agent.
- It points at **rendered** configs (Helm/Kustomize output). A step **outside the tool** fills the `rendered/` folder before each run: from Git (PR branch) in CI, from the running system (ConfigMaps / OpenTelemetryCollector resources, Mimir ruler, Grafana API) for continuous checks.
- **Staleness: both mechanisms**:
  - Validate on every run: referenced files exist; every join's exporter and receiver exist and are used in a pipeline; endpoints and protocols match. Failures make every path through that part `unverified`.
  - Store a `sha256` per referenced config. Hash differs but validation passes: trusted, with a warning to rerun the skill.
- **Strict**: every exporter must be joined or explicitly listed as external (e.g. `debug`). An unknown exporter fails validation.
- Shape (as implemented; `tap`, `env` and `feature_gates` are unconfirmed defaults, see below):

  ```yaml
  version: 1
  collectors:
    agent:
      config: rendered/otel-agent.yaml
      sha256: …
      tap: metrics/tap                 # the pipeline that records this collector's input
      snapshot_pipelines: [metrics/detected]   # pipelines that only record snapshot metadata; skipped like the tap
      feature_gates: [some.gate, -other.gate]   # as passed with --feature-gates
      env: { GATEWAY_ENDPOINT: "otel-gateway:4317" }   # values for ${env:VAR} in the config
    gateway: { config: rendered/otel-gateway.yaml, sha256: … }
  backends:
    mimir: { type: mimir-otlp, config: rendered/mimir-limits.yaml, sha256: … }
  consumers:
    rules:        { type: prometheus-rules, paths: [rules/], reads: mimir }
    alertmanager: { type: alertmanager, config: rendered/alertmanager.yaml, reads: rules }
    dashboards:   { type: grafana, paths: [dashboards/], datasources: { mimir-uid: mimir } }
  external: [agent/exporters/debug]
  joins:
    - { from: agent/exporters/otlp/gateway, to: gateway/receivers/otlp }
    - { from: gateway/exporters/otlphttp/mimir, to: mimir }
  ```

- **How a run follows it** (`internal/topology`):
  - Recorded items enter each tapped collector at its tap pipeline's receivers, and go through every other metrics pipeline using those receivers, processor by processor, in config order.
  - A pipeline's exporters: joined to a receiver → the next collector's pipelines using that receiver; joined to a backend → delivered to that backend; listed in `external` → an `exported` event, and the data leaves the modeled system.
  - Fan-out (a receiver feeding a second pipeline, a pipeline with a second exporter) copies the items with new IDs `c1, c2, ...`: node `topology`, action `derived`, `from` the original. The first branch keeps the original IDs.
  - Data crossing a join that validation couldn't trust still flows, so the rest of the path is checked, but every item gets an `unverified` event at the exporter (node `<collector>/<exporter id>`).
- **Validation** (`topology.Validate`), with three severities (unconfirmed default, see below):
  - **fatal** (the run stops): a config can't be loaded; no collector has a tap, or the tap pipeline doesn't exist; an unknown backend or consumer type; a consumer reading something that doesn't exist; a join whose exporter isn't defined or isn't used by a pipeline; an exporter in use that is neither joined nor external (decision 28); the same feature gate set differently on two collectors.
  - **unverified** (paths through the join are unverified): the exporter's port differs from the receiver's; the receiver doesn't enable the exporter's protocol (`otlp` = gRPC, `otlphttp` = HTTP); the receiver listens on localhost only (its default since collector 0.104 is `localhost:4317`/`4318`); non-OTLP exporters or receivers; a gRPC `otlp` exporter joined to Mimir, which takes OTLP over HTTP.
  - **warning** (reported, results trusted): a config's hash differs from the topology's `sha256`; a receiver feeds a pipeline but isn't tapped, so its data isn't in recordings; an endpoint holds an `${env:...}` reference without a value under `env`, so ports can't be compared.
  - Problems are in the report's `problems` section and printed by the CLI.
- **Feature gates** of all collectors are applied to the collector's process-wide registry before processors are built.

### Extensibility

- **Small core, everything else adapters** (proposed principle, not explicitly confirmed). Core: graph, signal model, lineage, report. Adapters: processors, backends, consumers.
- **Plugin model: compiled-in Go.** Adapters register a factory under a type name; a builder manifest picks which go into the binary, like the collector's own builder.

### Processing units

- Real processors run once per **unit**, never on whole batches: an `Item` (one data point, log record or span, with copies of its resource and scope) or a **metric** group. The runner splits and regroups; the envelope (`ID`, `From`) lives outside `pdata`.
- Matching outputs to inputs: one input means any output came from it (0 outputs: `dropped`; 1: same ID, diff attributes; 2+: new IDs with `From` = input). In a metric group, outputs that match no single input get the whole group as `From`.
- Adapters must pick the unit from config: e.g. transform/filter statements in metric context that look across data points (`HasAttrOnDatapoint`, `aggregate_on_attributes`), metricstransform `aggregate_labels` / `aggregate_label_values` / `combine` need the metric unit. A wrong declaration means silent drift, so adapters default to the metric unit when unsure.
- Not yet supported: **series** units (need stepped time; e.g. deltatocumulative running totals), **trace** units (tail sampling; span IDs would give exact matching), **batch** units (groupbyattrs, dedup). Processors needing them give `unverified`.
- Owner's processors: batch and memory_limiter (pass-through), attributes and resource (real code, item unit), filter, transform and metricstransform (real code, metric unit), deltatocumulative (real code, item unit at a single instant), resourcedetection (custom node, values from snapshot metadata), k8sattributes (custom node).

### Nodes built so far

- **`source/otlp-replay`** (`internal/nodes/source`): one item per data point, IDs `s1, s2, ...` in file order, origin `<file>#L<line>`.
- **`source/sdk-env`** (`internal/nodes/sdkenv`): as specified under Nodes. Also handles `OTEL_SERVICE_NAME` (it wins over `service.name` in `OTEL_RESOURCE_ATTRIBUTES`). Attribution compares the recorded `service.name` with each container's SDK service name. Malformed `OTEL_RESOURCE_ATTRIBUTES` gives `unverified`, since SDKs disagree on handling it.
- **`collector/k8sattributes`** (`internal/nodes/k8sattributes`): pod association (resource attributes `k8s.pod.ip`, `k8s.pod.uid`, `container.id`, name + namespace; `connection` can't match in a replay); never overwrites existing attributes; container attributes need `k8s.container.name`, `container.id` or a single-container pod. Extracts namespace, pod name/uid, node, owner names (ReplicaSet, Deployment via `pod-template-hash`, StatefulSet, DaemonSet, Job), container name/id. Other attributes give `unverified`. Defaults for `extract.metadata` and `pod_association` follow the docs. **Unmodeled settings (e.g. `filter`, `extract.labels`) make the whole node `Unsupported`**, rather than being ignored.
- **`backend/mimir-otlp`** (`internal/nodes/mimir`): runs Prometheus's OTLP translator per item with settings from Mimir limits (`promote_otel_resource_attributes`, `otel_metric_suffixes_enabled`, `otel_keep_identifying_resource_attributes`; unknown limits make the run fail to load). Legacy name validation, no delta temporality (rejected metrics get `dropped`). Reads the `limits` section of a full Mimir config and ignores the other sections; a limit it doesn't model is an error. A `runtime_config` file can override limits per tenant and isn't read yet, so its presence makes every item `unverified`. Outputs Prometheus-shaped series `p1, p2, ...`: data series first, then `target_info`; identical series from several inputs (e.g. `target_info` of one resource) become one series with several parents. Explains promotion in `derived` details and emits `skipped` for promoted attributes missing on the input.
- **`rule/<record>`** (`internal/nodes/rules`, `internal/promqlx`, `internal/rulefile`): recording rules from rule files (Prometheus's `rulefmt`), evaluated once at `eval_time` in dependency order (a rule sees the outputs of rules it reads), with the real PromQL engine over in-memory series. The explainer gives each output's parents and `dropped` events for selector near-misses (series with the selector's metric name that a matcher rejects; `requires` detail names the label) and for unmatched `and`/`on(...)` sides. Outputs `r1, r2, ...` ordered by first parent. When the explainer says an output is possible but the engine produced none at this instant (too few samples for `rate()`, or a value filter), the inputs get **`unverified`, not `dropped`**. Unsupported constructs: outputs still come from the engine, with every selected series as parents, plus an `unverified` event.
- **Explainer coverage** (`promqlx.Explain`): selectors, aggregations (`by`/`without`; topk-like pass through), label-keeping and name-dropping functions, `histogram_quantile`, `label_replace`, `label_join`, scalar and vector binary ops with `on`/`ignoring`/`group_left`/`group_right`, `and`/`or`/`unless`. Not yet: `count_values`, `absent*`, many-to-many, functions with several vector arguments. `TestExplainerAgreesWithEngine` checks it against the real engine on 25 expressions; add an expression there for every construct you add.
- **`alert/<alertname>`** (`internal/nodes/alerts`, `internal/alerting`): alerting rules evaluated like recording rules (shared `internal/ruleeval`), outputs are `alerting.Alert` values `a1, a2, ...` with series labels minus `__name__`, rule labels and `alertname`. Templated rule labels (`{{ ... }}`) aren't expanded yet: `unverified`. `derived` details carry `expr`, `evaluated_as` (after the rewrite) and `for`.
- **`alertmanager/route`** (`internal/nodes/route`): Alertmanager's own config loader and routing tree (`dispatch.Route.Match`, `continue` respected). `routed` events carry the receiver and the route path (matchers in Alertmanager's sorted order). A receiver without integrations gives `dropped`. Mute/active time intervals on the matched route and inhibit rules targeting the alert give `unverified` (not modeled yet). Silences are runtime state and out of scope.
- **Series keep all their samples** through the backend and rules, so range functions see the recorded history.
- **Real processors** (`internal/nodes/realproc`): attributes, deltatocumulative, filter, metricstransform, resource and transform, built from their collector-contrib 0.134 factories with the config section from the collector config (`confmap`, then the component's own `Validate`). An invalid or unknown setting makes the node `Unsupported`. Each unit runs through the real processor; the output is compared with the input:
  - Single-item units: same ID, with `set`/`removed` events for resource attributes, data point attributes (detail `level: datapoint`) and metric fields (`metric.name`, `metric.unit`, `metric.type`, `metric.temporality`); no output gives `dropped`.
  - Metric units: outputs are matched to inputs by data point (start time, timestamp, value). An output matching no input gets a new `c` ID with every input of the group as parents. `metricstransform`'s `combine` merges different metrics, which needs a batch unit: `unverified`.
  - **filter** names the first condition that matches a dropped item (`dropped by filter metric condition \`...\``), evaluated with the real OTTL library.
  - **transform** replays its statements one at a time with the real OTTL library (`resource`, `scope`, `metric` and `datapoint` contexts, standard OTTL functions): each change names the statement that made it (detail `statement`, `context`). A statement whose condition is false is re-run without its `where` clause on a scratch copy; what it would have changed becomes a `skipped` event (detail `condition: false`, `would_set`), unless another statement did set that attribute. The replay is checked against the real output: a mismatch gives `unverified`. Statements that can't be replayed (transform-only functions such as `convert_sum_to_gauge`, groups with `conditions`, statements without a context block) only lose their statement detail; the real output stays the truth.
  - Skipped events carry the statement's condition as `detail.where`. The report turns a skipped statement into one hint that says what to change: "Make the condition `...` in `gateway/transform/team` hold for this pod, or set `team` another way."
- **`<collector>/resourcedetection`** (`internal/nodes/resourcedetection`): applies values recorded in the snapshot (decision 40), for the Kubernetes node the signal's pod ran on (`k8s.pod.ip` → snapshot pod → `spec.nodeName`), else the `*` values. `override` (default true) is modeled: with it off, existing attributes are `kept`. No recorded values for the processor or the node, or detector settings that differ from the recorded config (anything but `override`): `unverified`.
- **batch, memory_limiter**: pass-through (they regroup or shed load, but don't change data). Any other processor type: `Unsupported` (data passes through, every item `unverified`).
- **Processor node names**: `<collector>/<processor id>`, or `<collector>/<pipeline>/<processor id>` when the processor is used by several metrics pipelines of that collector.
- Scenario runs: `internal/scenario.Run` validates the topology, then wires replay → sdk-env → the topology runner (collectors, following joins) → one backend node per backend → recording rules (metrics graph), then alerting rules and routing as separate stages logged with `graph.Record`.
- **Pods** (`internal/kube.LoadPods`) decode like the API server (`sigs.k8s.io/json`). Fields the 1.32 API types don't know, e.g. from a newer cluster, are ignored and reported as a `warning` problem; fixtures must have none (`TestFixturesHaveNoUnknownFields`).
- **Golden tests** (`internal/scenario/golden_test.go`) run every scenario in `testdata/thin-slice` and `testdata/agent-gateway` and compare whole reports exactly, event IDs and topology problems included. To change a golden report deliberately: `go test ./internal/scenario -update`, then review the diff and note why in that fixture directory's README.
- **Report** (`internal/report`): see Lineage and reports.
- **Flow graph** (`internal/flowgraph`, built by `topology.Flow` and `scenario.Run`): the scenario's components in flow order, with lineage node names as IDs (source, sdk-env, each modeled pipeline's processors and exporters following joins, backends, recording rules and alerts with their inputs, Alertmanager routes, receivers). Bad joins carry validation's reason on their edge. `scenario.ReportWithFlow` returns it with the report.
- **Scenario `description`**: one sentence saying what the scenario changes ("A PR adds a log-shipper sidecar to vllm-embed."), shown under its column and above each route on the report page. It isn't part of the JSON report, so golden reports don't carry it.
- **Report page** (`internal/htmlreport`, decision 49): `page.html` holds the styles and script; data is embedded as JSON (html/template escapes it, so config text can't end the script). The page shows the coverage matrix (scenarios as columns, real-stack verdicts when compare ran) and, for the selected cell, a strip map: one row per flow node, grouped by part of the stack, the route as a line that stops where it breaks, the root cause ringed, the pod's events beside each node, and event counts for all signals. Data is always rendered as text, never as HTML. The page embeds only what it shows (decision 50): each entry's path signals and the copies made from them, their events, the events the summary and diagnosis cite, and per-node event counts for the whole run. On the kind lab's two-hour recording that's 130 KB instead of 30 MB; `-json` keeps the full lineage.
- **CLI** (`cmd/otelcov`): `otelcov scenario [-root dir] [-json] [-html page.html] scenario.yaml...`; exit status 1 if anything is not covered, 2 on errors.

### Snapshot metadata

- `recorded_at` and `pods` (pod specs at recording time).
- `resource_detection` (decision 40; format is an unconfirmed default): by collector, then processor ID, the processor's `config` at recording time and the detected `values` per Kubernetes node name (daemonsets) or `*`:

  ```yaml
  resource_detection:
    agent:
      resourcedetection:
        config: { detectors: [env, system], override: false }
        values:
          kind-worker: { host.name: kind-worker, os.type: linux }
  ```
- **Capture** (decision 48): the agent runs a second recording pipeline, `metrics/detected`, with a copy of its resourcedetection processor (same settings, `override: true`). Detected values are what that pipeline's output has and the raw tap doesn't, matched by the sender's `k8s.pod.ip` (`dev/kind/snapshot`). The topology lists the pipeline under `snapshot_pipelines`, so runs and validation skip it.

### Lineage and reports

- **Structured events**: `{id, node, signal, attribute?, action, value?, simulated?, reason?, detail?}`.
- Actions: `attributed`, `associated`, `set`, `kept`, `removed`, `skipped`, `derived`, `dropped`, `routed`, `exported` (left the modeled system through an external exporter), `unverified` (the node can't tell what really happens; reason says why).
- Signal ID prefixes: `s` recorded, `c` copies and processor-made items, `p` stored series, `r` rule outputs, `a` alerts.
- **Record non-events** (`skipped`): in the motivating bug, the root cause is something that *didn't* happen.
- Report sections: `summary` (per requirement and pod: status, path, receiver or `broken_at`), `diagnosis` (event chain from the break back to the root cause, plus hints), `problems` (non-fatal problems in the inputs: topology validation, unknown pod fields), `signals`, `events`.
- Statuses: `covered`, `covered_simulated` (path needs an attribute only sdk-env provided; listed in `simulated_by`), `not_covered`, `unverified` (no telemetry from the pod, or an `unverified` event on the path; listed in `unverified_by`).
- How the report answers a requirement (`internal/report`):
  - **Sources**: recorded signals of the requirement's metric that sdk-env attributed to the pod and container (or, without attribution and without a required container, that k8sattributes associated with the pod). None: `unverified`, `broken_at: source/otlp-replay`.
  - **Forward search** through `from` links to the requirement's alert, routed to its receiver.
  - **Relevance**: attribute-level events only count if the path *needs* the attribute: it's promoted to a label that a selector matcher (one that doesn't match the empty string) or a route matcher on the path requires (`job` ← `service.name`, `instance` ← `service.instance.id`). So `k8s.pod.uid` being simulated doesn't make a path `covered_simulated` unless something needs it.
  - **Breaks**: the deepest non-`target_info` signal with a `dropped` event (else `unverified`) gives `broken_at`. An alert routed elsewhere breaks at `alertmanager/route`.
  - **Diagnosis**: from the drop, the missing label → the backend's `skipped` promotion of the attribute it comes from (label names via Prometheus's own `LabelNamer`) → the last collector or sdk-env event on the recorded signal that skipped, removed or couldn't verify that attribute. Explanations are generated from those events; hints are specific for known root causes (k8sattributes container identification, sdk-env removal, a transform statement whose condition is false) and generic otherwise. Identifiers in explanations and hints are quoted in backticks; the report page shows them as code. Hints say what to change, without restating the explanation.

### Time

- **Single instant** for now: `for:` is treated as met, rules evaluated once. Move to stepped time (walking recorded timestamps, like promtool's test format) only when a check needs it.
- Replays must use **timestamps from the data**, never arrival time; a replay reads files as fast as it can.

## Decision log

| # | Decision | Alternatives considered | Why |
|---|---|---|---|
| 1 | Cross-layer coverage checker | Proving operators correct (Anvil-style), model checking (TLA+/Kivi-style), SMT checks of configs | Fills the real gap (no tool checks one layer against another); usable in CI |
| 2 | Cover the entire monitoring stack as one graph | Label-provenance checker for the collector only | Generality |
| 3 | Checks written in Go | Datalog, Rego, graph DB (Cypher) | Quickest start; same language as the reused Prometheus/Alertmanager/collector code |
| 4 | Every component is a node mapping input → output | — | Owner's model; mostly a DAG with a few stateful nodes |
| 5 | Graph starts at collector input (receivers, scrape targets) | Start at Mimir/Loki | Collector what-ifs need pre-collector data |
| 6 | Tap inside the collector | Re-collect data ourselves; mix | One mechanism for every receiver type |
| 7 | Staging/test only | — | Don't load prod |
| 8 | Checks run continuously; snapshots replayable for CI | Snapshot + CI only | Always fresh |
| 9 | Missing staging series → `unverified` and reported | Fail; generate synthetic traffic | Honest without false alarms |
| 10 | Record raw telemetry (file exporter) | Dump deduplicated inventory | Keeps values and timestamps |
| 11 | Mask log bodies by pattern | Keep bodies; drop bodies | Most log rules stay checkable without storing secrets |
| 12 | Build a thin end-to-end slice first | Collector side first; rules first | Proves the graph design early |
| 13 | Slice: hand-written fixtures, lineage, single instant | kind cluster / EKS lab data; diff-only; stepped time | Fast, deterministic; stepped time when needed |
| 14 | Structured lineage events | Free-text notes | Lets the tool link causes automatically |
| 15 | Rule explanation via AST, reusing `promql/parser` | Leave-one-out re-evaluation | Scales; names the missing label directly |
| 16 | Simulate SDK env vars (sdk-env node) | Accept "unverified until re-recorded"; SDKs send `container.id` | Pre-merge answers for env var fixes |
| 17 | sdk-env attribution: `service.name` match, then only instrumented container | Either alone | Precise first, fallback second |
| 18 | sdk-env syntax: literals + pod fieldRefs + `$(VAR)` | Literals only | Covers the downward API pattern |
| 19 | Processor nodes: real code + comparing wrapper + OTTL statement events; custom k8sattributes | Custom models everywhere; real code + wrapper only | Matches production, keeps the "why" where it matters |
| 20 | Dashboard variables: explicit lists in requirements | Wildcards; resolve via variable queries | Simple now; resolve later |
| 21 | Open source, as generic as possible | — | No existing tool does this |
| 22 | Plugin model: compiled-in Go | gRPC plugins; WebAssembly | Reuses collector-contrib factories; the others may come later |
| 23 | Topology discovery by an agent skill writing a topology file | Built-in auto-discovery; hand-written only | Handles many patterns; file stays reviewable |
| 24 | First release: owner's stack only | Plus plain Prometheus path; breadth-first parse-only | Something real fastest |
| 25 | Thin topology file | Thick (every node spelled out) | Collector configs already describe pipelines; PR changes to processors are seen automatically |
| 26 | Staleness: validate every run + config hashes | Either alone | Correctness plus a cheap rerun signal |
| 27 | Fetch/render outside the tool | Built-in fetchers per adapter | Tool stays simple for now |
| 28 | Strict: unknown exporters fail validation | Lenient (treat as leaving the system) | Clear error instead of every pod silently "not covered" |
| 29 | Name: `otelcov` | `plumbline`, `blindspot` (`sightline` is taken by an IT monitoring vendor) | Descriptive and searchable. Trademark check (2026-10-23): "OpenTelemetry" is a registered Linux Foundation mark, "OTel" isn't listed; the LF guidelines discourage marks or their abbreviations in product names (the approved form is "<name> for OpenTelemetry"), but many community tools use "otel" in their names. The owner kept the name |
| 30 | Write our own LogQL parser | Real Loki as a separate process; AGPL license; metrics-only first release | Loki's `pkg/logql` (parser and engine) is AGPL; our own parser keeps a permissive license possible. Evaluation of filters/parsers comes later; until then they're `unverified` |
| 31 | License: Apache-2.0 | MIT; AGPL-3.0 | Matches the collector, Prometheus and Alertmanager; CNCF default; explicit patent grant |
| 32 | LogQL parser built with a parser generator (goyacc) | Hand-written recursive descent | Declarative grammar, easy to compare with the LogQL docs; can switch later |
| 33 | LogQL conformance: tests from the docs and our own queries, plus comparison against a real Loki in CI | Either alone | Catches differences nobody thought to test |
| 34 | Development builds run in Claude's cloud workspace; whatever can't build there runs in GitHub Actions | Owner's Mac; widening the workspace's network allowlist | No setup for the owner |
| 35 | Signals are collector `pdata` throughout (`pmetric`, `plog`, `ptrace`), including after the backend node | Own types; hybrid (pdata until the backend, own types after) | Real processors run without conversion; tying otelcov to collector versions fits decision 19's version pinning |
| 36 | Dependencies are vendored by CI onto `deps/<branch>` branches | CI compiles everything; build Go/modules from GitHub mirrors; widen the network allowlist | Fast offline builds in the dev workspace; downloads stay checksum-verified against sum.golang.org |
| 37 | Pin collector **0.134** (`pdata` v1.40.0, Go 1.24), the version the owner's collectors run. The kind lab runs the `0.134.1` image: the v0.134.0 release has no binaries, and v0.134.1 rebuilt the same contrib v0.134.0 components | 0.135 (used until 2026-10-23); latest collector (needs Go 1.26) | Match the deployed collectors' behavior |
| 38 | Lineage across real processors: run them per item, with IDs and parents kept in an envelope outside `pdata` (Writer-monad style: each node returns outputs plus events) | Hidden ID attribute on each item; matching outputs to inputs by content | Exact matching, and processors can't strip or react to the metadata |
| 39 | Processing units: **item** and **metric** (all data points of one metric in one resource and scope). Adapters choose the unit from their config; an output in a metric group that matches no single input gets the whole group as parents | `unverified` for anything needing more than one item; `unverified` now, units later | Covers metricstransform aggregations and cross-point transform/filter statements in the owner's configs |
| 40 | resourcedetection values come from snapshot metadata: each collector instance's detected attributes are captured at recording time | Read them from Mimir; "present, value unknown" | Real values; a detector added by a PR gives `unverified` until a new snapshot exists |
| 41 | Kubernetes objects use the real API types (`k8s.io/api`, `apimachinery`, same versions as collector-contrib 0.134) | Own minimal structs | Exact semantics; nodes, ConfigMaps and Deployments will be needed |
| 42 | Code layout: `internal/{lineage,unit,graph,kube,otelenv,collector,scenario,nodes/...}`, one package per node; nodes are built with their config and take data in `ProcessMetrics` | — | Mirrors the collector (typed pipelines, config at construction) |
| 43 | After the backend node, series are **Prometheus-shaped** `pmetric`: Prometheus name, all labels as data-point attributes, empty resource | OTLP-shaped, translated when queried | Matches what Mimir stores; rule outputs fit the same shape; trivial conversion for PromQL |
| 44 | Alerts are a small Go type of our own, the one exception to decision 35 | `plog` log records | Alerts have state over time and a fixed schema; no collector code runs on them, and Alertmanager needs plain label sets anyway |
| 45 | Prometheus and Alertmanager at the versions collector-contrib 0.134 uses (the same as 0.135) (Prometheus commit `419d436a447a`, Alertmanager v0.28.1); OTLP → series uses Prometheus's own translator (`prometheusremotewrite`) | Mimir's fork of the translator (AGPL, can't be imported) | Real code; versions consistent with the collector pin |
| 46 | Coverage ignores values: rules (recording and alerting) are rewritten so comparisons become `bool` and `topk`/`bottomk`/`limitk`/`limit_ratio` pass their input through, then evaluated with the real engine; `for` is treated as met | Strip comparisons and evaluate the left side; explainer only | Owner's point: thresholds don't change *whether* a series is watched, only labels and matching do. The rewrite keeps the real engine as the check on the explainer. Whether thresholds are *realistic* (units, impossible values) is a separate, later check |
| 47 | Next after the thin slice: agent + gateway topology support, and real collector-contrib processor nodes with OTTL statement explanations; then test against a kind cluster running that topology | Stepped time; Loki/LogQL; dashboards | The owner's stack is agents + gateway, and the processors are where coverage silently breaks |
| 48 | kind lab (`dev/kind`, Tilt): collectors as plain manifests; Mimir + Alertmanager + a webhook sink as ground truth to compare otelcov's answers with; a small Python OTel SDK app as the vLLM stand-in; resourcedetection values captured by a second agent pipeline after it | OTel Helm chart or Operator; collectors only (no ground truth) or Mimir only; telemetrygen or real vLLM on CPU; deriving detections in the capture script | otelcov reads the same files that are deployed; real firing alerts check the predictions; full control over metric names and temporality; detection values come from the real processor |
| 49 | Report UI: one static, self-contained HTML page (`otelcov scenario -html`, and `compare -html` in the lab) showing a coverage matrix across scenarios, each pod's route (lineage) on the pipeline graph, and real vs predicted; no build step (Go `html/template`, plain JavaScript, data embedded as JSON) | A local web server (`otelcov serve`); both; a React app with a build step | Opens anywhere, works as a CI/PR artifact, nothing new to install |
| 50 | The report page embeds only what it shows: route signals and their copies, their events, the events the summary and diagnosis cite, and per-node event counts for the whole run | A shorter capture window in the lab; both | A real recording has an event per data point per node (30 MB for two hours of the kind lab); routes need a few dozen. The lab's capture stays as it is |
| 51 | The k8sattributes model is held to upstream's own tests for feature parity: the cases in collector-contrib's `processor/k8sattributesprocessor/processor_test.go` and `internal/kube/client_test.go` at the pinned version. How they're brought in and kept in sync is open (see Open questions) | Hand-written cases only (as so far) | The model is hand-written (decision 19), and checking it against the 0.134 source found a divergence the hand-written tests missed (see Known gaps). Contrib is Apache-2.0 like otelcov, so its tests can be copied with attribution; the AGPL clean room doesn't apply |

## Defaults in the fixtures, not yet confirmed by the owner

Treat these as open; ask before building on them.

- **Pod association for replays**: the tap runs `k8sattributes` in `passthrough` mode so recordings carry `k8s.pod.ip` (a file has no client connection). Alternative: SDKs set `k8s.pod.uid` via the downward API and the tap stays processor-free.

Chosen while building decision 47 (the owner was away); each is easy to change:

- **Topology file additions**: `tap` names the tap pipeline (the draft had `tap: true`); `env` gives values for `${env:VAR}` references (unresolved ones stay as written and give a warning); `feature_gates` per collector.
- **Validation severities** (see Topology). **This deviates from decision 26**, which says validation failures make paths `unverified`: missing configs, joins from undefined or unused exporters, and conflicting feature gates are **fatal** instead, because the run can't model those collectors at all. Mismatched ports and protocols follow decision 26 (`unverified`).
- **Conflicting feature gates are fatal**: one otelcov process has one gate registry, so two collectors can't have different settings for the same gate.
- **Bad joins still pass data**, with an `unverified` event per item, so problems further down the path are still found.
- **Fan-out copies**: new `c` IDs, node `topology`, action `derived`; the first branch keeps the original IDs. `exported` is a new action for external exporters.
- **Processor node names**: `<collector>/<processor id>`, with the pipeline added only when the processor is shared by several pipelines.
- **memory_limiter is a pass-through**, like batch. Under real memory pressure it refuses data; that's runtime state, not config.
- **transform, filter and metricstransform always run in the metric unit**, rather than choosing the unit from their statements (decision 39 allows either). Item-level behavior is the same; it only costs precision when a group's outputs can't be matched to single inputs.
- **resource_detection snapshot format** (see Snapshot metadata), keyed by collector then processor ID, with the processor's config recorded so changed detector settings give `unverified`. A gateway's values should be under `*`: per-node keys are looked up by the *app* pod's node, which is only right for daemonsets.
- **Statement replay**: a `skipped` statement is hidden when another statement set the same attribute, so the diagnosis doesn't blame a statement that didn't matter.

Chosen while building the kind lab (decision 48):

- **`snapshot_pipelines`** as the topology field naming the agent's detection pipeline.
- **Pods from newer clusters** load with unknown fields ignored and reported as a `warning`. kind's current node images are 1.34+, while otelcov's API types are 1.32 (decision 41), so strict loading would refuse every real capture. Alternative: make those paths `unverified`.
- **The report's `topology` section is now `problems`**, since unknown pod fields go there too.
- **Mimir config**: only the `limits` section is read, and a limit otelcov doesn't model is still an error. A `runtime_config` file makes every item `unverified`.
- **Capture**: the snapshot's `eval_time` is the recording's last data point timestamp (rounded up to the second). A cut-off last line in a tap file is skipped. Each capture replaces the previous one.
- **compare's verdicts**: covered agrees only if the alert is routed to the required receiver; not_covered agrees if there's no alert in the ruler or Alertmanager; unverified is `n/a`. Only scenarios whose collector configs equal the deployed ConfigMaps are compared.
- **kind-only settings**: the alert threshold is `> 0` with `for: 1m`, the rule interval is 30s, and Alertmanager waits are short, so the real stack answers within minutes.
- **Apps what-ifs in the lab** (`dev/kind/snapshot/whatif.go`): a manifest's Deployment templates are applied to the captured pods they own (ReplicaSet `<deployment>-<pod-template-hash>`). Only container env/envFrom and pod labels change; a template with a different set of containers is refused. This is a first, lab-only answer to the open question on PR what-ifs for Deployment templates.
- **compare and replaced pods**: a pod that no longer exists gives `n/a` (capture again). A pod that exists but has no series in Mimir is a disagreement when otelcov said covered.

Chosen while building the report page (decision 49):

- **Design**: one plane separated by rules, Atkinson Hyperlegible Next and Mono (loaded from Google Fonts, with system fallbacks offline; contextual alternates off so IDs render as typed), status colors always paired with words, light and dark. The route is drawn as a strip map rather than a free-form graph, because the flow is mostly a chain; side branches (e.g. copies to `debug`) get an elbow.
- **What the route shows**: the requirement's path signals plus copies made from them. A node is on the route if the signal acted there or it lies between such nodes, so pass-through processors appear hollow. The page opens on the URL's `#scenario|requirement|pod`, else the first result that isn't covered.
- **Report schema details**: signal ID prefixes (`s`, `p`, `r`, `a`), field names, the `covered_simulated` status.
- **Repository layout**: fixtures in `testdata/` (Go convention).

## Planned for later

- **Stepped time** (`for:`, `rate()` over real timestamps), when a check needs it.
- **Resolve dashboard variables** by running their own queries against the data.
- **gRPC and WebAssembly plugins** as extension mechanisms.
- **Built-in fetchers** (Helm render, ConfigMaps, Mimir ruler, Grafana API).
- **Deterministic topology discovery** in code, next to the skill.
- **More stacks**: plain Prometheus (OTLP receiver, remote write), other backends and consumers, other collector deployment patterns (sidecars, load-balancing exporter, queues like Kafka between collectors).
- **OTel Weaver live-check** as a possible source-side node (does the app emit what its schema says?).

## Known gaps

- **k8sattributes and init containers** (found 2026-10-24 by reading contrib 0.134's `processor.go` and `internal/kube/client.go`): the real processor falls back to "the pod's only container" by counting containers from `status.containerStatuses` **plus `status.initContainerStatuses`** (and spec containers plus init containers). otelcov's `identifyContainer` counts `spec.containers` only. So for a pod with one app container and one init container (e.g. `istio-init`, a wait-for-db step, a native sidecar), the real processor doesn't set `k8s.container.name` but otelcov does, and can report the pod **covered when it isn't**. The kind lab didn't catch it: its pods have no init containers. To fix together with decision 51.
- **A renamed metric leaves no trace** (found 2026-10-24): the rule explainer only records a drop for a near miss, a series with the selector's metric name that fails a matcher. If the metric reaches Mimir under another name (e.g. `otel_metric_suffixes_enabled` turned off, so `process_memory_usage` instead of `process_memory_usage_bytes`; a translation strategy change; a `metricstransform` rename), the selector ignores the series like any unrelated one. The pod comes out `unverified` with "the signal reaches no alert and nothing explains why": honest, but no diagnosis. Possible fix: also count a series as a near miss when its name differs only by a unit or type suffix, or when it comes from the same OTLP metric as the selected name would. Related: the CLI prints a dangling `✗` on the path line when there's no `broken_at`.
- **Group matching can swap data points** (found 2026-10-28): transform, filter and metricstransform run on all of one metric's points from one app, and outputs are matched to inputs by start time, timestamp and value, taking ties in order. When two points share those (common: `up = 1`, counters at `0`, anything scraped at the same instant) and the processor drops or reorders one, the wrong input is matched. Reproduced with `filter` dropping pod a's point by a data point attribute: otelcov reported pod a's point as kept with `pod` changed to `b`, and pod b's point as dropped. That can make a dropped pod look covered. Possible fixes: include point attributes in the match and fall back to timestamp and value only when attributes changed; for filter, decide which inputs are dropped first by evaluating its conditions per input (the drop explanation already does); or mark ambiguous matches `unverified` instead of matching by order.
- **Some transform configs give no `skipped` event**: statement groups with `conditions`, and statements without a context block (the shorter style recent collector docs show), aren't replayed. The real processor still runs and its changes are reported, but a statement that didn't run leaves no trace, so a diagnosis that depends on it stops one step early ("… which nothing sets for this pod") instead of naming the statement and its condition. Transform-only functions (`convert_sum_to_gauge`, `aggregate_on_attributes`, ...) aren't replayed either; their changes are reported as "changed by the transform processor", without the statement.
- **Smaller k8sattributes differences** from the same reading: the real processor strips the `containerd://`-style prefix from pod status container IDs only, not from the resource's `container.id` (otelcov strips both); and it overwrites an attribute that is present but empty (otelcov keeps it).

## Open questions

- **Bringing upstream k8sattributes tests in and keeping them in sync** (decision 51). Upstream tests come in two layers: `processor_test.go` feeds the processor pre-built cache entries (`kube.Pod{Containers: ByName/ByID}`), and `internal/kube/client_test.go` checks how a Kubernetes pod becomes those entries. otelcov's model goes from a pod straight to attributes, so each case has to be translated, and Go can't import another module's `_test.go` files or `internal/` packages, so the tests can't be called directly. Options:
  - **Port with provenance, plus a drift check**: each case keeps the upstream file, test and case name and the version; a test reads the upstream test files from the Go module cache at the pinned version (module downloads include `_test.go` files) and fails when their hash differs from the one recorded at the last port, naming the changed tests, so a version bump forces a re-port.
  - **Reference the upstream files directly**: no copies; our tests point at upstream cases by name and the module-cache files are the source of truth. Still needs each case translated in code, so in practice it's the drift check without the copies.
  - **Differential testing against the real processor**: run the real k8sattributes processor too (it needs a Kubernetes API, e.g. a fake API server serving the pods) on the same inputs, including every upstream case, and compare outputs. The real code is the oracle and stays in sync with the pin by itself; the most work.

- **PR what-ifs on Deployment templates**: fixtures keep pod names and IPs stable. A real PR changes a template, so the checker must map recorded pods to their owner and apply the new template.
- **LogQL evaluation**: when and how to evaluate line filters and parsers (`|= "error"`, `| json`); until then they're `unverified`.
- **Snapshot metadata contents**: pod specs and resourcedetection values today; which other configs (collector config SHAs, ServiceMonitors) to store.
- **Per-instance differences between agents**: the runner treats every agent instance as running the same config; only resourcedetection values differ per node.
- **Version pinning mechanism** for collector-contrib (builder manifest field, check against deployed version?).
- **Real Mimir configs** carry many limits that don't affect OTLP (ingestion rates, series limits), and otelcov refuses any limit it doesn't model. It needs a policy: ignore a known list of non-OTLP limits, or only check the `otel_*` / `promote_otel_*` ones. Mimir also has OTLP limits otelcov doesn't model yet: `otel_translation_strategy`, `otel_promote_scope_metadata`, `otel_label_name_underscore_sanitization`, `otel_label_name_preserve_multiple_underscores`, `otel_convert_histograms_to_nhcb`, `otel_native_delta_ingestion`, `otel_created_timestamp_zero_ingestion_enabled`.
- **k8sattributes `filter`** (e.g. `node_from_env_var`, common on daemonset agents) makes otelcov's node `Unsupported`. The kind agent leaves it out; work agents probably set it.
- **A definite port mismatch** (exporter and receiver ports differ) is `unverified` per decision 26. The kind lab shows the real result is a total loss; arguably that's `not_covered`.
- **Repository conventions**: Go module path, CI setup, contribution guidelines.

## Gotchas and facts worth knowing

- **k8sattributes**: container-level attributes need `k8s.container.name` or `container.id` on the incoming resource, or a pod with one container, counting init containers (see Known gaps). Checked against contrib 0.134's source: it doesn't overwrite attributes already present, unless their value is empty.
- **Prometheus/Mimir OTLP ingestion**: only promoted resource attributes become labels (Mimir: `promote_otel_resource_attributes`, comma-separated string); the rest go to `target_info`. Dots become underscores; `service.name` becomes `job`.
- **Unverified**: the Mimir setting name `otel_metric_suffixes_enabled` (adds unit/type suffixes, e.g. `process_memory_usage_bytes`). Check against the deployed Mimir version.
- **Loki OTLP ingestion**: some resource attributes become stream labels, the rest structured metadata. A stream selector on a structured-metadata attribute matches nothing.
- **File exporter**: `format` is `json` or `proto`; `rotation` has `max_megabytes`, `max_days`, `max_backups`, `localtime`; `append` and `rotation` can't be combined; whether `compression: zstd` works with JSON is unconfirmed.
- **ServiceMonitor relabelings** are already applied by the time data reaches the tap, so a snapshot can't test a PR that changes them. With the OTel Operator's Target Allocator, its API lists every scrape target; relabeling can run on Prometheus's own relabel package.
- **Changes left of the tap** (SDK env vars, app instrumentation) are baked into recordings; only sdk-env can simulate them.
- **Real processors don't explain non-events**; that's why k8sattributes is custom, and why transform statements are replayed one at a time to find the ones whose condition was false.
- **Transform-only OTTL functions** (`convert_sum_to_gauge`, `extract_sum_metric`, ...) live in the transform processor, not in `pkg/ottl`'s standard functions, so the statement replay can't run them; the processor itself still does.
- **OTLP receiver default endpoints** are `localhost:4317` (gRPC) and `localhost:4318` (HTTP) since collector 0.104, so a receiver config without `endpoint` isn't reachable from other pods.
- **Build environment**: Claude's cloud workspace has Go 1.24 and can fetch modules hosted on GitHub via `GOPROXY=direct`, but not `proxy.golang.org`, `sum.golang.org`, the Go toolchain downloads, or custom import paths (`golang.org/x`, `go.opentelemetry.io`, `google.golang.org`, `gopkg.in`, `k8s.io`). `pdata` v1.69.0 needs Go 1.26 and several of those paths.
- **Collector compatibility**: `pdata` (v1) follows Go's compatibility promise and OTLP is wire-stable, but most collector-contrib modules are v0 and component *output* can change between versions (alpha/beta components, feature gates flipping). Hence the version pin.
- **Feature gates are config.** Collectors run with `--feature-gates`; nodes must apply the same gate settings, or processors behave differently. The topology file lists them per collector; otelcov applies them to the process-wide registry, so two collectors can't set one gate differently.
- **One binary links one version of each module.** If agent and gateway run different collector-contrib versions, one otelcov build can only match one of them.
- **promtool unit tests** compare alert annotations exactly; keep test annotations deterministic.
- **Mimir rings and memberlist advertise the host's interface address.** In a pod that's the pod IP, which is fine. Where there's no private IP (Claude's workspace), memberlist fails to start; pass `-memberlist.advertise-addr=127.0.0.1` and the ring/frontend `instance-addr` flags to 127.0.0.1.
- **Mimir 3.x** still supports the classic architecture (ingest storage is off by default). `-target=all` includes the ruler but not the Alertmanager. With `ruler_storage.backend: local`, rules live at `<directory>/<tenant>/<file>`, and the tenant is `anonymous` when multitenancy is off. The ruler's Alertmanager URL is `-ruler.alertmanager-url` (the YAML `ruler.alertmanager_url` is deprecated).
- **`go test ./internal/scenario -update` rewrites every golden report** in sorted key order, including unchanged ones. Limit it with `-run 'TestGoldenReports/<dir>/<scenario>'`, or restore the files that only changed order.

## Prior art

| Tool | What it does | What it doesn't |
|---|---|---|
| Anvil (OSDI '24) | Verifies Kubernetes controllers in Rust/Verus with machine-checked proofs | Monitoring, telemetry |
| Kivi (ATC '24) | Model-checks Kubernetes controllers and their configs | Monitoring, telemetry |
| Cluster Guardian | Flags unscraped Services and missing alerts for known stateful services, by heuristics | Per-workload paths, pipelines |
| Gremlin scenarios | Injects resource spikes and checks monitoring notices | Explanations, pre-merge checks |
| Bindplane | Snapshot Recordings at a processor, replayed through processor changes (Live Preview) | Stops at the collector; Bindplane-managed only; no coverage |
| OTTL Playground (Elastic) | Real collector code via WebAssembly for transform/filter, before/after diffs | Single processor, manual |
| OTel Weaver live-check | Checks emitted telemetry against semantic conventions or a custom registry | Pipelines, alerts |
| mimirtool analyze | Metrics used by dashboards/rules vs metrics in Mimir | Labels, per-workload view, pipelines |
| OTelBin | Visualizes and validates collector configs | Data |
| pint, promtool | Rule linting and unit tests | Pipelines, per-workload coverage |

## Commands

### Building offline (dev workspace)

`vendor/` is never committed to source branches. The `vendor` workflow runs on every push that touches Go files or `go.mod`, and publishes `go.mod`, `go.sum` and `vendor/` on `deps/<branch>`.

```sh
git fetch origin '+refs/heads/deps/main:refs/remotes/origin/deps/main'
git checkout origin/deps/main -- go.mod go.sum vendor && git reset -q go.mod go.sum vendor
GOFLAGS=-mod=vendor go test ./...
```

- To use a new dependency: add it to `go.mod` and to `internal/deps/tools.go`, push, wait for the workflow, fetch again, then commit the `go.mod`/`go.sum` from the deps branch (they're verified by CI).
- With normal network access (e.g. on a laptop or in CI), plain `go test ./...` works; vendoring isn't needed.

### Running

```sh
GOFLAGS=-mod=vendor go run ./cmd/otelcov scenario testdata/thin-slice/scenarios/*.yaml
GOFLAGS=-mod=vendor go run ./cmd/otelcov scenario testdata/agent-gateway/scenarios/*.yaml

# kind lab (owner's laptop): see dev/kind/README.md
kind create cluster --config dev/kind/kind.yaml && tilt up -f dev/kind/Tiltfile

# After editing a config on purpose, refresh its sha256 in the topology file
sha256sum testdata/agent-gateway/collector/*.yaml
```

### Fixtures

```sh
# Real-tool checks of the fixtures (they pass; see Status)
cd testdata/thin-slice
promtool check rules rules/inference.rules.yaml
(cd promtool && promtool test rules inference.rules.test.yaml)
amtool check-config alertmanager/alertmanager.yaml
amtool config routes test --config.file=alertmanager/alertmanager.yaml \
  --verify.receivers=inference-pager alertname=VLLMMemoryHigh severity=page team=inference
otelcol-contrib validate --config=collector/collector.yaml
```

## Layout

```
AGENTS.md                 this file
README.md                 human-facing overview
LICENSE                   Apache-2.0
cmd/otelcov               the CLI
go.mod, go.sum            module github.com/Anefu/otelcov
internal/deps/tools.go    packages pinned for vendoring (build tag `tools`)
internal/lineage          signal IDs, events, the run's log
internal/unit             items, metric groups and Prometheus-shaped series items over pdata
internal/graph            node interfaces, runner, PassThrough, Unsupported
internal/kube             loading pods, env var resolution (downward API, $(VAR))
internal/otelenv          SDK resource from OTEL_RESOURCE_ATTRIBUTES / OTEL_SERVICE_NAME
internal/collector        minimal collector config reader, ${env:VAR} substitution
internal/topology         topology file, validation, the runner that follows joins
internal/promqlx          real PromQL engine over in-memory series; the label-flow explainer
internal/rulefile         rule files (rulefmt), dependency order
internal/ruleeval         one expression for coverage: rewrite, engine, explainer, parents
internal/alerting         the Alert type
internal/requirements     coverage requirements (OTel terms)
internal/report           summary, diagnosis
internal/flowgraph        a scenario's components and how they connect, for the report page
internal/htmlreport       the self-contained report page (page.html)
internal/scenario         scenario loading, runs, golden tests
internal/nodes/...        one package per node; nodetest has test helpers
internal/nodes/realproc   real collector-contrib processors, OTTL statement and condition explanations
.github/workflows/ci.yml      gofmt, tidy, vet, tests, fixture check
.github/workflows/vendor.yml  vendors dependencies onto deps/<branch>
testdata/thin-slice/      fixtures for the first slice; see its README
testdata/agent-gateway/   daemonset agents + gateway fixtures; see its README
dev/kind/                 kind lab: Tiltfile, manifests, configs, demo app, capture/snapshot/compare; see its README
```
