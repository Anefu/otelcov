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

Design phase. No Go code yet. Repository: `github.com/Anefu/otelcov`, Apache-2.0.

- `testdata/thin-slice/`: hand-written fixtures for the first end-to-end slice (4 scenarios, golden reports). See its README.
- `scripts/check_fixtures.py`: stopgap consistency check for the fixtures; delete once Go tests exist.
- The fixtures have **not** yet been run through the real tools (promtool, amtool, otelcol validate). See Commands.

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
- Draft shape:

  ```yaml
  version: 1
  collectors:
    agent:   { config: rendered/otel-agent.yaml, sha256: …, tap: true }
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

### Extensibility

- **Small core, everything else adapters** (proposed principle, not explicitly confirmed). Core: graph, signal model, lineage, report. Adapters: processors, backends, consumers.
- **Plugin model: compiled-in Go.** Adapters register a factory under a type name; a builder manifest picks which go into the binary, like the collector's own builder.

### Lineage and reports

- **Structured events**: `{id, node, signal, attribute?, action, value?, simulated?, reason?, detail?}`.
- Actions: `attributed`, `associated`, `set`, `kept`, `skipped`, `derived`, `dropped`, `routed`.
- **Record non-events** (`skipped`): in the motivating bug, the root cause is something that *didn't* happen.
- Report sections: `summary` (per requirement and pod: status, path, receiver or `broken_at`), `diagnosis` (event chain from the break back to the root cause, plus hints), `signals`, `events`.
- Statuses: `covered`, `covered_simulated` (path needs an attribute only sdk-env provided; listed in `simulated_by`), `not_covered`, `unverified`.

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
| 29 | Name: `otelcov` | `plumbline`, `blindspot` (`sightline` is taken by an IT monitoring vendor) | Descriptive and searchable; check Linux Foundation trademark rules for "otel" in names |
| 30 | Write our own LogQL parser | Real Loki as a separate process; AGPL license; metrics-only first release | Loki's `pkg/logql` (parser and engine) is AGPL; our own parser keeps a permissive license possible. Evaluation of filters/parsers comes later; until then they're `unverified` |
| 31 | License: Apache-2.0 | MIT; AGPL-3.0 | Matches the collector, Prometheus and Alertmanager; CNCF default; explicit patent grant |
| 32 | LogQL parser built with a parser generator (goyacc) | Hand-written recursive descent | Declarative grammar, easy to compare with the LogQL docs; can switch later |
| 33 | LogQL conformance: tests from the docs and our own queries, plus comparison against a real Loki in CI | Either alone | Catches differences nobody thought to test |
| 34 | Development builds run in Claude's cloud workspace; whatever can't build there runs in GitHub Actions | Owner's Mac; widening the workspace's network allowlist | No setup for the owner |
| 35 | Signals are collector `pdata` throughout (`pmetric`, `plog`, `ptrace`), including after the backend node | Own types; hybrid (pdata until the backend, own types after) | Real processors run without conversion; tying otelcov to collector versions fits decision 19's version pinning |

## Defaults in the fixtures, not yet confirmed by the owner

Treat these as open; ask before building on them.

- **Pod association for replays**: the tap runs `k8sattributes` in `passthrough` mode so recordings carry `k8s.pod.ip` (a file has no client connection). Alternative: SDKs set `k8s.pod.uid` via the downward API and the tap stays processor-free.
- **"Covered" means watched**: the pod's series reaches the alert's input, threshold ignored. A promtool test shows it matches real firing with bad values.
- **Golden-test strictness**: compare `summary` and `diagnosis` exactly; for `events`, require expected events to be present and ignore extras.
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

## Open questions

- **PR what-ifs on Deployment templates**: fixtures keep pod names and IPs stable. A real PR changes a template, so the checker must map recorded pods to their owner and apply the new template.
- **LogQL evaluation**: when and how to evaluate line filters and parsers (`|= "error"`, `| json`); until then they're `unverified`.
- **Snapshot metadata contents**: pod specs today; which other configs (collector config SHAs, ServiceMonitors) to store.
- **Version pinning mechanism** for collector-contrib (builder manifest field, check against deployed version?).
- **Repository conventions**: Go module path, CI setup, contribution guidelines.

## Gotchas and facts worth knowing

- **k8sattributes**: container-level attributes need the pod to have one container, or `container.id` / `k8s.container.name` on the incoming resource. Assumed (verify in tests): it doesn't overwrite attributes already present.
- **Prometheus/Mimir OTLP ingestion**: only promoted resource attributes become labels (Mimir: `promote_otel_resource_attributes`, comma-separated string); the rest go to `target_info`. Dots become underscores; `service.name` becomes `job`.
- **Unverified**: the Mimir setting name `otel_metric_suffixes_enabled` (adds unit/type suffixes, e.g. `process_memory_usage_bytes`). Check against the deployed Mimir version.
- **Loki OTLP ingestion**: some resource attributes become stream labels, the rest structured metadata. A stream selector on a structured-metadata attribute matches nothing.
- **File exporter**: `format` is `json` or `proto`; `rotation` has `max_megabytes`, `max_days`, `max_backups`, `localtime`; `append` and `rotation` can't be combined; whether `compression: zstd` works with JSON is unconfirmed.
- **ServiceMonitor relabelings** are already applied by the time data reaches the tap, so a snapshot can't test a PR that changes them. With the OTel Operator's Target Allocator, its API lists every scrape target; relabeling can run on Prometheus's own relabel package.
- **Changes left of the tap** (SDK env vars, app instrumentation) are baked into recordings; only sdk-env can simulate them.
- **Real processors don't explain non-events**; that's why k8sattributes is custom.
- **Build environment**: Claude's cloud workspace has Go 1.24 and can fetch modules hosted on GitHub via `GOPROXY=direct`, but not `proxy.golang.org`, `sum.golang.org`, the Go toolchain downloads, or custom import paths (`golang.org/x`, `go.opentelemetry.io`, `google.golang.org`, `gopkg.in`, `k8s.io`). `pdata` v1.69.0 needs Go 1.26 and several of those paths.
- **promtool unit tests** compare alert annotations exactly; keep test annotations deterministic.

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

```sh
# Fixture consistency (stopgap, until Go tests exist)
python3 scripts/check_fixtures.py testdata/thin-slice

# Real-tool checks of the fixtures (not run yet)
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
scripts/check_fixtures.py stopgap fixture checker (delete once Go tests exist)
testdata/thin-slice/      fixtures for the first slice; see its README
```
