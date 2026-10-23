# kind lab

A small cluster running the owner's topology for real, to check otelcov against it:

```
vllm-chat (1 container) ─┐                       ┌─▶ Mimir 3.2.3 ── ruler ──▶ Alertmanager 0.28.1 ──▶ alert-sink
vllm-embed (+ sidecar) ──┴─▶ agent ──▶ gateway ──┤   (OTLP, limits)   (rules)   (routes)               (logs who got paged)
          Python OTel SDK      daemonset   0.135  └─▶ debug
                            tap ▶ recordings
```

- **Agent** (daemonset, one per worker): k8sattributes, resourcedetection, resource, batch. Two extra pipelines exist only for otelcov: `metrics/tap` records raw input, and `metrics/detected` records what resourcedetection adds.
- **Gateway**: memory_limiter, filter (drops `debug.heartbeat`), transform (sets `team`), deltatocumulative, batch.
- **Apps**: a Python stand-in for vLLM sends `process.memory.usage`, `debug.heartbeat` and a delta `vllm.requests` every 15s.
- The rule, alert and routes match `testdata/agent-gateway`. The alert threshold is `> 0` and `for: 1m`, so it really fires; thresholds don't change otelcov's answer (decision 46).

The loop: let it run, **capture** what the agents recorded, run **otelcov** on the capture, then **compare** its answers with what Mimir and Alertmanager actually did.

## You'll need

Docker (Docker Desktop or colima, with about 4 GB of memory), [kind](https://kind.sigs.k8s.io/), kubectl, [Tilt](https://docs.tilt.dev/install.html) and Go 1.24 or newer. On a Mac:

```sh
brew install kind kubectl tilt-dev/tap/tilt go
```

All images are multi-arch, so Apple Silicon works. The first `tilt up` pulls about 1 GB of images and builds otelcov, which downloads its Go modules once.

## Run it

From the repository root:

```sh
kind create cluster --config dev/kind/kind.yaml   # context kind-otelcov, 2 workers
tilt up -f dev/kind/Tiltfile                      # UI at http://localhost:10350
```

When everything is green, give it about 2 minutes: apps export every 15s, the rule runs every 30s, and the alert needs 1 minute to fire. Then check by hand if you like:

- Mimir's *rule output* and *ALERTS* links on the `mimir` resource (port-forwarded to `localhost:9009`).
- The `alert-sink` logs: one line per alert, with the receiver it was routed to (`inference-pager`).

Then, in the Tilt UI (or `tilt trigger <name>`), run the three manual resources under **otelcov**, in order:

1. **capture** copies each agent's recordings off the cluster, plus the pods and the deployed agent config, into `dev/kind/captured/`. It builds `recording.otlp.jsonl` and `snapshot.meta.yaml`, with resourcedetection values per node, and writes the scenarios from `otelcov/scenarios/` with `eval_time` set to the last recorded timestamp. Each capture replaces the previous one.
2. **otelcov** runs all three scenarios on the capture:
   - `01-baseline` (what's deployed): both pods covered.
   - `02-team-pr` (the gateway PR in `whatif/`): both not covered, with the transform statement as the root cause.
   - `03-agent-badport` (the agent config in `whatif/`): both unverified, with a topology problem for the port.

   Each also warns about pod fields otelcov doesn't know. That's expected: kind runs a newer Kubernetes than the 1.32 API types otelcov is built with.
3. **compare** checks otelcov's answer against Mimir and Alertmanager for each pod. It only does this for scenarios whose collector configs are the ones deployed right now (read from the ConfigMaps) and skips the others. `agree` is what you want. `wait` means the alert is pending or not routed yet.

## Check a what-if for real

otelcov predicts a PR's effect from the old recording. To check that prediction, deploy the PR and compare again:

```sh
tilt args -- --gateway=whatif/gateway-team-pr.yaml   # the gateway restarts with the PR
# wait ~6 minutes: old series stay visible for 5 (the lookback window), then the ruler resolves the alert
# run compare again (no new capture needed): 02-team-pr is now the deployed one; expect "agree" for both pods
tilt args --clear                                    # back to config/
```

`--agent=whatif/agent-badport.yaml` breaks the agent → gateway join for real: nothing reaches Mimir. otelcov answers `unverified` (it can't tell, and says why), so compare shows `n/a`.

Editing `config/*.yaml` redeploys too. otelcov's topology (`otelcov/topology.yaml`) points at the same files, so its next run sees the edit. For a prediction *before* deploying, put the change in a new file under `whatif/` with its own topology and scenario template, as the two examples do.

## Files

| Path | What it is |
|---|---|
| `kind.yaml` | Cluster: one control plane, two workers |
| `Tiltfile` | Deploys everything; makes ConfigMaps from `config/` (or the `--agent`/`--gateway` variants) and restarts workloads when their config changes; builds otelcov and the two tools below |
| `config/` | What's deployed: agent and gateway collector configs, Mimir config (otelcov reads its `limits`), rules, Alertmanager routes |
| `whatif/` | Configs for what-ifs: the gateway team-condition PR, the agent with the wrong port |
| `k8s/` | Manifests: agent daemonset (with a `tap-reader` sidecar, since the collector image has no shell), gateway, Mimir (with a PersistentVolumeClaim), Alertmanager, alert-sink, apps |
| `app/` | The Python stand-in for vLLM and the webhook sink (one image) |
| `otelcov/` | otelcov's view: topology files (with `snapshot_pipelines: [metrics/detected]`), the requirement, scenario templates |
| `capture.sh` | Step 1 above |
| `snapshot/` | Turns raw captures into `recording.otlp.jsonl`, `snapshot.meta.yaml` and scenarios. Its test runs the whole lab offline: a fake capture through otelcov with the real configs |
| `compare/` | Step 3 above |
| `captured/`, `bin/` | Generated; ignored by git |

## Already checked without a cluster

- Every collector config passes `otelcol-contrib validate` (0.135.0). The Alertmanager config passes `amtool check-config`, and both routes resolve as intended.
- `tilt alpha tiltfile-result` evaluates the Tiltfile, including the `--gateway` variant, and puts every object in the intended resource.
- The backend half ran locally with the real binaries (Mimir 3.2.3, Alertmanager 0.28.1, the 0.135 gateway, the sink), fed with what the agents would forward:
  - The ruler loaded the rules from the mounted layout, alerts fired, and both were routed to `inference-pager`.
  - compare agreed with otelcov.
  - The series labels Mimir stored matched otelcov's model of its OTLP translation exactly, `target_info` included.
  - After switching the gateway to the PR config and waiting for staleness, compare agreed that both pods lost coverage.

**Not checked yet** (that's tonight):
- kind itself, k8sattributes against a real API server, and pod association by connection IP through the `otel-agent` Service.
- The Python app and image build, the `kubectl` parts of `capture.sh`, and Tilt actually running.

## Troubleshooting

- **Tilt refuses to start**: it only runs against `kind-otelcov`. Create the cluster, or `kubectl config use-context kind-otelcov`.
- **capture says there are no data points**: the apps haven't exported yet; wait a minute.
- **compare says `DISAGREE: no alert (or the data is older than 5m)`**: compare looks at the stack *now*. If the apps stopped, the series went stale.
- **Mimir data**: kept on the `mimir-data` volume across restarts. To start fresh, `tilt down` and `kubectl --context kind-otelcov -n observability delete pvc mimir-data`.
- **Tear down**: `tilt down`, then `kind delete cluster --name otelcov`.
