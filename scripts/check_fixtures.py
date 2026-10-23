"""Independent consistency check for the thin-slice fixtures.

Usage: python3 scripts/check_fixtures.py testdata/thin-slice

Re-derives each scenario's coverage from the raw inputs (snapshot pod specs, pods,
recording, collector, backend, rules, routes, requirements) and compares it with the
golden report. Also checks file references, OTLP/JSON shape, report integrity and
that the promtool test matches the reports.

This is a stopgap that encodes a tiny model of the thin slice only. It is NOT the
checker. Delete it once Go tests run the real nodes against these fixtures.
"""
import json
import re
import sys
from pathlib import Path

import yaml

ROOT = Path(sys.argv[1])
errors = []


def check(cond, msg):
    if not cond:
        errors.append(msg)


def load_yaml(p):
    return yaml.safe_load(Path(p).read_text())


# 1. Every YAML / JSON file parses
for p in ROOT.rglob("*"):
    try:
        if p.suffix in (".yaml", ".yml"):
            yaml.safe_load(p.read_text())
        elif p.suffix == ".json":
            json.loads(p.read_text())
        elif p.name.endswith(".jsonl"):
            for line in p.read_text().splitlines():
                json.loads(line)
    except Exception as e:
        errors.append(f"parse {p}: {e}")


# 2. OTLP/JSON shape of recordings
def check_otlp(path):
    for i, line in enumerate(path.read_text().splitlines(), 1):
        req = json.loads(line)
        check(set(req) == {"resourceMetrics"}, f"{path.name}:{i} top-level keys {set(req)}")
        for rm in req["resourceMetrics"]:
            for a in rm["resource"]["attributes"]:
                check(set(a) == {"key", "value"} and list(a["value"]) == ["stringValue"],
                      f"{path.name}:{i} attribute shape {a}")
            for sm in rm["scopeMetrics"]:
                check("name" in sm["scope"], f"{path.name}:{i} scope without name")
                for m in sm["metrics"]:
                    data = [k for k in ("gauge", "sum", "histogram", "exponentialHistogram", "summary") if k in m]
                    check(len(data) == 1, f"{path.name}:{i} metric {m['name']} has data fields {data}")
                    s = m.get("sum", {})
                    check(s.get("aggregationTemporality") in (1, 2), f"{path.name}:{i} aggregationTemporality")
                    for dp in s.get("dataPoints", []):
                        for k in ("startTimeUnixNano", "timeUnixNano", "asInt"):
                            check(isinstance(dp.get(k), str) and dp[k].isdigit(),
                                  f"{path.name}:{i} {k} must be a digit string, got {dp.get(k)!r}")
                        check(int(dp["startTimeUnixNano"]) < int(dp["timeUnixNano"]),
                              f"{path.name}:{i} start time not before time")


for rec in (ROOT / "recordings").glob("*.jsonl"):
    check_otlp(rec)


# 3. Env var resolution (literals, fieldRef, $(VAR)) and OTEL_RESOURCE_ATTRIBUTES parsing
FIELD_PATHS = {
    "metadata.uid": lambda pod: pod["metadata"]["uid"],
    "metadata.name": lambda pod: pod["metadata"]["name"],
    "metadata.namespace": lambda pod: pod["metadata"]["namespace"],
    "spec.nodeName": lambda pod: pod["spec"]["nodeName"],
    "status.podIP": lambda pod: pod["status"]["podIP"],
}


def resolve_env(pod, container):
    env = {}
    for e in container.get("env", []):
        if "value" in e:
            env[e["name"]] = re.sub(r"\$\((\w+)\)", lambda m: env.get(m.group(1), m.group(0)), e["value"])
        elif "fieldRef" in e.get("valueFrom", {}):
            env[e["name"]] = FIELD_PATHS[e["valueFrom"]["fieldRef"]["fieldPath"]](pod)
        else:
            env[e["name"]] = None  # unsupported source -> unverified
    return env


def env_attrs(pod, container):
    raw = resolve_env(pod, container).get("OTEL_RESOURCE_ATTRIBUTES")
    if not raw:
        return {}
    check("$(" not in raw, f"unresolved reference in {raw}")
    return dict(kv.split("=", 1) for kv in raw.split(","))


def attribute_container(pod, res):
    by_name = [c for c in pod["spec"]["containers"]
               if resolve_env(pod, c).get("OTEL_SERVICE_NAME") == res.get("service.name")]
    if len(by_name) == 1:
        return by_name[0], "service.name"
    instrumented = [c for c in pod["spec"]["containers"] if any(k.startswith("OTEL_") for k in resolve_env(pod, c))]
    if len(instrumented) == 1:
        return instrumented[0], "only instrumented container"
    return None, "ambiguous"


# 4. Independent derivation per scenario
def parse_selector(expr):
    m = re.search(r"(\w+)\{([^}]*)\}", expr)
    return m.group(1), dict(re.findall(r'(\w+)\s*=\s*"([^"]*)"', m.group(2)))


def parse_by(expr):
    return [x.strip() for x in re.search(r"by\s*\(([^)]*)\)", expr).group(1).split(",")]


def route(am, labels):
    for r in am["route"].get("routes", []):
        ms = dict(re.match(r'(\w+)="([^"]*)"', x).groups() for x in r["matchers"])
        if all(labels.get(k) == v for k, v in ms.items()):
            return r["receiver"]
    return am["route"]["receiver"]


def derive(scen_path):
    sc = load_yaml(scen_path)
    base = scen_path.parent

    def ref(p, rel=base):
        q = (rel / p).resolve()
        check(q.exists(), f"{scen_path.name}: missing file {p}")
        return q

    rec = ref(sc["data"]["recording"])
    meta_path = ref(sc["data"]["snapshot"])
    check(meta_path.name.split(".")[0] == rec.name.split(".")[0], f"{scen_path.name}: snapshot/recording mismatch")
    old_pods = load_yaml(ref(load_yaml(meta_path)["pods"], meta_path.parent))["items"]
    pods = load_yaml(ref(sc["config"]["pods"]))["items"]
    col = load_yaml(ref(sc["config"]["collector"]))
    backend = load_yaml(ref(sc["config"]["backend"]))["limits"]
    rules = load_yaml(ref(sc["config"]["rules"][0]))
    am = load_yaml(ref(sc["config"]["alertmanager"]))
    reqs = load_yaml(ref(sc["requirements"]))["requirements"]
    expected = json.loads(ref(sc["expected"]).read_text())
    check(expected["scenario"] == sc["name"], f"{scen_path.name}: scenario name mismatch")

    pipeline = col["service"]["pipelines"][sc["config"]["collector_pipeline"]]
    check(pipeline["processors"] == ["k8sattributes", "batch"], f"{scen_path.name}: unexpected processors")
    k8s = col["processors"]["k8sattributes"]
    check(k8s["pod_association"][0]["sources"][0] == {"from": "resource_attribute", "name": "k8s.pod.ip"},
          "association source changed")
    extract = k8s["extract"]["metadata"]
    promote = [x.strip() for x in backend["promote_otel_resource_attributes"].split(",")]
    old_by_ip = {p["status"]["podIP"]: p for p in old_pods}
    new_by_name = {p["metadata"]["name"]: p for p in pods}
    by_ip = {p["status"]["podIP"]: p for p in pods}

    series, simulated = {}, {}
    for line in rec.read_text().splitlines():
        rm = json.loads(line)["resourceMetrics"][0]
        res = {a["key"]: a["value"]["stringValue"] for a in rm["resource"]["attributes"]}
        metric = rm["scopeMetrics"][0]["metrics"][0]

        # sdk-env: snapshot spec -> config spec
        old_pod = old_by_ip.get(res.get("k8s.pod.ip"))
        check(old_pod is not None, f"{scen_path.name}: no snapshot pod for {res.get('k8s.pod.ip')}")
        container, how = attribute_container(old_pod, res)
        check(container is not None, f"{scen_path.name}: can't attribute signal from {old_pod['metadata']['name']}"
                                     f" ({how}) -> would be unverified")
        if container is None:
            continue
        old_attrs = env_attrs(old_pod, container)
        for k, v in old_attrs.items():
            check(res.get(k) == v, f"{scen_path.name}: recording disagrees with snapshot env for {k}: "
                                   f"{res.get(k)!r} vs {v!r}")
        new_pod = new_by_name[old_pod["metadata"]["name"]]
        new_container = next(c for c in new_pod["spec"]["containers"] if c["name"] == container["name"])
        new_attrs = env_attrs(new_pod, new_container)
        sim = set()
        for k, v in old_attrs.items():
            if k not in new_attrs and res.get(k) == v:
                del res[k]
                sim.add(k)
        for k, v in new_attrs.items():
            if old_attrs.get(k) != v:
                res[k] = v
                sim.add(k)

        # k8sattributes against the config's pods
        pod = by_ip.get(res.get("k8s.pod.ip"))
        check(pod is not None, f"{scen_path.name}: recording pod ip {res.get('k8s.pod.ip')} has no pod")
        vals = {"k8s.namespace.name": pod["metadata"]["namespace"], "k8s.pod.name": pod["metadata"]["name"],
                "k8s.pod.uid": pod["metadata"]["uid"], "k8s.node.name": pod["spec"]["nodeName"]}
        for attr in extract:
            if attr in res:
                continue
            if attr == "k8s.container.name":
                if len(pod["spec"]["containers"]) == 1:
                    res[attr] = pod["spec"]["containers"][0]["name"]
            else:
                res[attr] = vals[attr]

        # backend
        check(metric["name"] == "process.memory.usage" and metric["unit"] == "By"
              and backend["otel_metric_suffixes_enabled"], "metric naming assumptions changed")
        labels = {"__name__": "process_memory_usage_bytes", "job": res["service.name"]}
        label_source = {"job": "service.name"}
        for attr in promote:
            if attr in res:
                labels[attr.replace(".", "_")] = res[attr]
                label_source[attr.replace(".", "_")] = attr
        series[pod["metadata"]["name"]] = (labels, int(metric["sum"]["dataPoints"][0]["asInt"]), label_source)
        simulated[pod["metadata"]["name"]] = sim

    # rules + routes
    rec_rule = next(r for r in rules["groups"][0]["rules"] if "record" in r)
    alert_rule = next(r for r in rules["groups"][0]["rules"] if "alert" in r)
    sel_name, matchers = parse_selector(rec_rule["expr"])
    by = parse_by(rec_rule["expr"])
    check(alert_rule["expr"].split()[0] == rec_rule["record"], "alert does not read the recording rule")
    derived = {}
    for pod_name, (labels, value, label_source) in series.items():
        out = {"p": labels, "value": value}
        if labels["__name__"] == sel_name and all(labels.get(k) == v for k, v in matchers.items()):
            out["r"] = {"__name__": rec_rule["record"], **{k: labels[k] for k in by}}
            out["a"] = {"alertname": alert_rule["alert"], **{k: labels[k] for k in by}, **alert_rule["labels"]}
            out["receiver"] = route(am, out["a"])
            out["needs_simulated"] = sorted({label_source[k] for k in matchers} & simulated[pod_name])
        derived[pod_name] = out

    # requirements -> compare with the golden summary
    req = reqs[0]
    sel = req["for_each_pod"]["selector"]
    targets = [p for p in pods if p["metadata"]["namespace"] == req["for_each_pod"]["namespace"]
               and all(p["metadata"]["labels"].get(k) == v for k, v in sel.items())]
    exp_summary = {s["pod"]: s for s in expected["summary"]}
    sig = expected["signals"]
    ev = {e["id"]: e for e in expected["events"]}
    for p in targets:
        key = f"{p['metadata']['namespace']}/{p['metadata']['name']}"
        d = derived.get(p["metadata"]["name"])
        if d is None:
            check(False, f"{scen_path.name}: {key} derived as unverified, golden says {exp_summary[key]['status']}")
            continue
        reaches = d.get("receiver") == req["must_reach"]["receiver"]
        want = ("covered_simulated" if d.get("needs_simulated") else "covered") if reaches else "not_covered"
        e = exp_summary.get(key)
        check(e is not None, f"{scen_path.name}: no summary for {key}")
        if not e:
            continue
        check(e["status"] == want, f"{scen_path.name}: {key} status {e['status']}, derived {want}")
        path = e["path"]
        check(sig[path[1]]["labels"] == d["p"], f"{scen_path.name}: {key} series labels differ:\n"
              f"  expected {sig[path[1]]['labels']}\n  derived  {d['p']}")
        check(sig[path[1]]["value"] == d["value"], f"{scen_path.name}: {key} value differs")
        if reaches:
            check(len(path) == 4 and sig[path[2]]["labels"] == d["r"], f"{scen_path.name}: {key} rule output")
            check(len(path) == 4 and sig[path[3]]["labels"] == d["a"], f"{scen_path.name}: {key} alert labels")
            check(e.get("receiver") == d["receiver"], f"{scen_path.name}: {key} receiver differs")
            sims = [ev[x] for x in e.get("simulated_by", []) if x in ev]
            check(sorted(x["attribute"] for x in sims) == d["needs_simulated"]
                  and all(x["node"] == "source/sdk-env" and x.get("simulated") for x in sims),
                  f"{scen_path.name}: {key} simulated_by doesn't match {d['needs_simulated']}")
        else:
            check(len(path) == 2 and "r" not in d, f"{scen_path.name}: {key} should stop at the rule")
            missing = [k for k in matchers if k not in d["p"]]
            diag = [x for x in expected["diagnosis"] if x["pod"] == key]
            check(len(diag) == 1, f"{scen_path.name}: {key} needs exactly one diagnosis")
            if diag:
                check([diag[0]["missing_label"]] == missing,
                      f"{scen_path.name}: missing label {diag[0]['missing_label']} vs derived {missing}")
                check(len(p["spec"]["containers"]) > 1, f"{scen_path.name}: {key} root cause assumes 2+ containers")
    return sc["name"], derived


derived_by_scenario = {}
for scen in sorted((ROOT / "scenarios").glob("*.yaml")):
    name, d = derive(scen)
    derived_by_scenario[name] = d

# 5. Report integrity
for rp in (ROOT / "expected").glob("*.json"):
    r = json.loads(rp.read_text())
    sig, events = r["signals"], r["events"]
    ids = [e["id"] for e in events]
    check(len(ids) == len(set(ids)), f"{rp.name}: duplicate event ids")
    by_id = {e["id"]: e for e in events}
    for sid, s in sig.items():
        for f in s.get("from", []):
            check(f in sig, f"{rp.name}: signal {sid} from unknown {f}")
    for e in events:
        check(e["signal"] in sig, f"{rp.name}: event {e['id']} on unknown signal {e['signal']}")
    for s in r["summary"]:
        for a, b in zip(s["path"], s["path"][1:]):
            check(sig[b].get("from") == [a], f"{rp.name}: path {s['path']} broken between {a} and {b}")
    for dg in r["diagnosis"]:
        chain = [by_id.get(x) for x in dg["chain"]]
        check(all(chain), f"{rp.name}: diagnosis references unknown events")
        check(chain[0]["action"] == "dropped" and chain[-1]["action"] == "skipped",
              f"{rp.name}: chain should run from drop back to skip")
        check(dg["root_cause"] == dg["chain"][-1] and by_id[dg["root_cause"]]["node"] == "collector/k8sattributes",
              f"{rp.name}: root cause")

# 6. promtool test agrees with the reports
pt = load_yaml(ROOT / "promtool" / "inference.rules.test.yaml")


def parse_series(s):
    name, body = re.match(r"([\w:]+)\{(.*)\}", s).groups()
    return {"__name__": name, **dict(re.findall(r'(\w+)="([^"]*)"', body))}


for test, scen in ((pt["tests"][0], "01-single-container"), (pt["tests"][0], "03-sidecar-fixed"),
                   (pt["tests"][0], "04-fix-pr-simulated"), (pt["tests"][1], "02-sidecar-added")):
    d = derived_by_scenario[scen]
    got = sorted(json.dumps(parse_series(s["series"]), sort_keys=True) for s in test["input_series"])
    want = sorted(json.dumps(v["p"], sort_keys=True) for v in d.values())
    check(got == want, f"promtool '{test['name']}' input series differ from {scen}")
    exp = sorted(json.dumps(parse_series(x["labels"]), sort_keys=True)
                 for x in test["promql_expr_test"][0]["exp_samples"])
    check(exp == sorted(json.dumps(v["r"], sort_keys=True) for v in d.values() if "r" in v),
          f"promtool '{test['name']}' expected samples differ from {scen}")
fires = [a["exp_labels"]["k8s_pod_name"] for a in pt["tests"][2]["alert_rule_test"][0]["exp_alerts"]]
check(fires == [k for k, v in derived_by_scenario["02-sidecar-added"].items() if "a" in v],
      "promtool alert test disagrees with 02")

if errors:
    print(f"{len(errors)} problem(s):")
    for e in errors:
        print(" -", e)
    sys.exit(1)
for name, d in derived_by_scenario.items():
    print(name, {k[:10]: ("NOT covered" if "a" not in d[k] else
                          "covered (simulated: " + ",".join(d[k]["needs_simulated"]) + ")"
                          if d[k]["needs_simulated"] else "covered") for k in d})
print("all checks passed")
