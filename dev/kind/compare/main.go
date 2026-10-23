// Command compare checks otelcov's answers against the real kind stack.
//
// For each scenario whose collector configs are the ones deployed right now,
// it runs otelcov, then asks Mimir and Alertmanager what really happened to
// each pod: which series reached Mimir, whether the alert is pending or
// firing in the ruler, and which receivers Alertmanager routed it to. Then it
// says, per pod, whether otelcov's status agrees.
//
// Scenarios whose configs aren't deployed are skipped: their answer is a
// prediction for a config the cluster isn't running. Deploy it (tilt args) and
// wait for old series to go stale (~5 minutes) to check it.
//
//	compare [-mimir URL] [-alertmanager URL] [-html page.html] captured/scenarios/*.yaml
//
// With -html it also writes otelcov's report page for every scenario, with the
// real stack's answer next to otelcov's for the scenarios it compared.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Anefu/otelcov/internal/htmlreport"
	"github.com/Anefu/otelcov/internal/report"
	"github.com/Anefu/otelcov/internal/requirements"
	"github.com/Anefu/otelcov/internal/scenario"
	"github.com/Anefu/otelcov/internal/topology"
)

// deployed says where each collector's config lives in the cluster: the
// ConfigMaps the Tiltfile makes.
var deployed = map[string]configMap{
	"agent":   {"observability", "otel-agent-config", "agent.yaml"},
	"gateway": {"observability", "otel-gateway-config", "gateway.yaml"},
}

type configMap struct{ namespace, name, key string }

func main() {
	var o options
	flag.StringVar(&o.mimir, "mimir", "http://localhost:9009", "Mimir HTTP address (Tilt port-forward)")
	flag.StringVar(&o.alertmanager, "alertmanager", "http://localhost:9093", "Alertmanager address (Tilt port-forward)")
	flag.StringVar(&o.context, "context", "kind-otelcov", "kubectl context, to read the deployed configs")
	flag.StringVar(&o.podLabel, "pod-label", "k8s_pod_name", "label that carries the pod name on series and alerts")
	flag.StringVar(&o.namespaceLabel, "namespace-label", "k8s_namespace_name", "label that carries the namespace")
	htmlOut := flag.String("html", "", "also write the report page, with the real stack's answers, to this file")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: compare [flags] scenario.yaml...")
		os.Exit(2)
	}
	page := &htmlreport.Data{Generated: time.Now().UTC().Format(time.RFC3339)}
	disagree, err := run(context.Background(), o, flag.Args(), os.Stdout, page)
	if err == nil && *htmlOut != "" {
		err = writePage(*htmlOut, *page)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "compare:", err)
		os.Exit(2)
	}
	if disagree {
		os.Exit(1)
	}
}

type options struct {
	mimir, alertmanager, context, podLabel, namespaceLabel string
	// configMapData reads a deployed config, and podExists checks a pod is
	// still there; tests replace them.
	configMapData func(ctx context.Context, cm configMap) (string, error)
	podExists     func(ctx context.Context, namespace, name string) (bool, error)
}

func writePage(path string, d htmlreport.Data) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := htmlreport.Write(f, d); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// run compares each scenario that is deployed, printing a table per scenario,
// and fills page (if not nil) with every scenario's report and truth.
func run(ctx context.Context, o options, paths []string, w io.Writer, page *htmlreport.Data) (bool, error) {
	if o.configMapData == nil {
		o.configMapData = o.kubectlConfigMap
	}
	if o.podExists == nil {
		o.podExists = o.kubectlPodExists
	}
	disagree, compared := false, 0
	for _, path := range paths {
		s, err := scenario.Load(path, filepath.Dir(filepath.Dir(path)))
		if err != nil {
			return false, err
		}
		rep, flow, err := scenario.ReportWithFlow(ctx, s)
		if err != nil {
			return false, fmt.Errorf("%s: %w", s.Name, err)
		}
		sc := htmlreport.Scenario{Description: s.Description, Report: rep, Flow: flow}
		why, err := notDeployed(ctx, o, s)
		if err != nil {
			return false, err
		}
		if why != "" {
			fmt.Fprintf(w, "%s: skipped, not what's deployed (%s)\n\n", s.Name, why)
			sc.NotCompared = "not what's deployed (" + why + ")"
		} else {
			compared++
			d, truths, err := compare(ctx, o, s, rep, w)
			if err != nil {
				return false, fmt.Errorf("%s: %w", s.Name, err)
			}
			disagree = disagree || d
			sc.Truth = truths
		}
		if page != nil {
			page.Scenarios = append(page.Scenarios, sc)
		}
	}
	if compared == 0 {
		fmt.Fprintln(w, "No scenario matches the deployed configs. Capture again after deploying.")
	}
	return disagree, nil
}

// notDeployed returns why a scenario's collector configs differ from the
// deployed ones, or "".
func notDeployed(ctx context.Context, o options, s *scenario.Scenario) (string, error) {
	topo, err := topology.Load(s.Config.Topology)
	if err != nil {
		return "", err
	}
	for _, name := range topology.Names(topo.Collectors) {
		cm, ok := deployed[name]
		if !ok {
			return "", fmt.Errorf("don't know where collector %s's config is deployed", name)
		}
		want, err := os.ReadFile(topo.Collectors[name].Config)
		if err != nil {
			return "", err
		}
		got, err := o.configMapData(ctx, cm)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(got) != strings.TrimSpace(string(want)) {
			return fmt.Sprintf("the %s config differs from %s", name, topo.Rel(topo.Collectors[name].Config)), nil
		}
	}
	return "", nil
}

func (o options) kubectlConfigMap(ctx context.Context, cm configMap) (string, error) {
	key := strings.ReplaceAll(cm.key, ".", `\.`)
	out, err := exec.CommandContext(ctx, "kubectl", "--context", o.context, "-n", cm.namespace,
		"get", "configmap", cm.name, "-o", "jsonpath={.data."+key+"}").Output()
	if err != nil {
		return "", fmt.Errorf("reading ConfigMap %s/%s: %w", cm.namespace, cm.name, err)
	}
	return string(out), nil
}

func (o options) kubectlPodExists(ctx context.Context, namespace, name string) (bool, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "--context", o.context, "-n", namespace,
		"get", "pod", name, "--ignore-not-found", "-o", "name").Output()
	if err != nil {
		return false, fmt.Errorf("looking up pod %s/%s: %w", namespace, name, err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// truth is what the real stack did with one pod's signal.
type truth struct {
	gone      bool     // the pod no longer exists (redeployed since the capture)
	metrics   []string // series names in Mimir for the pod
	ruler     string   // "", "pending" or "firing"
	receivers []string // where Alertmanager routed the alert
}

func compare(ctx context.Context, o options, s *scenario.Scenario, rep *report.Report, w io.Writer) (bool, map[string]htmlreport.Truth, error) {
	reqs, err := requirements.Load(s.Requirements)
	if err != nil {
		return false, nil, err
	}
	truths := map[string]htmlreport.Truth{}
	byID := map[string]requirements.Requirement{}
	for _, r := range reqs {
		byID[r.ID] = r
	}

	fmt.Fprintf(w, "%s (otelcov evaluated at %s; the stack now)\n", s.Name, s.EvalTime)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  POD\tOTELCOV\tREAL STACK\tVERDICT")
	disagree := false
	for _, e := range rep.Summary {
		req := byID[e.Requirement]
		ns, pod, _ := strings.Cut(e.Pod, "/")
		t, err := o.lookup(ctx, ns, pod, req.MustReach.Alert)
		if err != nil {
			return false, nil, err
		}
		v := verdict(e.Status, t, req.MustReach.Receiver)
		truths[htmlreport.TruthKey(e.Requirement, e.Pod)] = htmlreport.Truth{Real: t.String(), Verdict: v}
		disagree = disagree || strings.HasPrefix(v, "DISAGREE")
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", e.Pod, e.Status, t, v)
	}
	if err := tw.Flush(); err != nil {
		return false, nil, err
	}
	fmt.Fprintln(w)
	return disagree, truths, nil
}

func (t truth) String() string {
	if t.gone {
		return "pod no longer exists"
	}
	var parts []string
	if len(t.metrics) == 0 {
		parts = append(parts, "no series in Mimir")
	} else {
		parts = append(parts, "series: "+strings.Join(t.metrics, ", "))
	}
	switch {
	case len(t.receivers) > 0:
		parts = append(parts, "alert routed to "+strings.Join(t.receivers, ", "))
	case t.ruler != "":
		parts = append(parts, "alert "+t.ruler+" in the ruler")
	default:
		parts = append(parts, "no alert")
	}
	return strings.Join(parts, "; ")
}

// verdict compares otelcov's status with the real stack.
func verdict(status string, t truth, receiver string) string {
	routed := false
	for _, r := range t.receivers {
		routed = routed || r == receiver
	}
	alerting := t.ruler != "" || len(t.receivers) > 0
	if t.gone {
		return "n/a: the pod was replaced since the capture; capture again"
	}
	switch status {
	case report.Covered, report.CoveredSimulated:
		switch {
		case routed:
			return "agree"
		case len(t.receivers) > 0:
			return "DISAGREE: routed elsewhere"
		case t.ruler != "":
			return "wait: not routed yet (for: 1m, group_wait 10s)"
		case len(t.metrics) == 0:
			return "DISAGREE: no series in Mimir (or the data is older than 5m)"
		default:
			return "DISAGREE: no alert"
		}
	case report.NotCovered:
		if alerting {
			return "DISAGREE: the alert exists"
		}
		return "agree"
	default:
		return "n/a: otelcov can't tell (" + status + ")"
	}
}

func (o options) lookup(ctx context.Context, namespace, pod, alert string) (truth, error) {
	var t truth
	exists, err := o.podExists(ctx, namespace, pod)
	if err != nil {
		return t, err
	}
	if !exists {
		t.gone = true
		return t, nil
	}
	sel := fmt.Sprintf(`%s=%q,%s=%q`, o.namespaceLabel, namespace, o.podLabel, pod)

	res, err := o.query(ctx, fmt.Sprintf(`count by (__name__) ({%s})`, sel))
	if err != nil {
		return t, err
	}
	for _, r := range res {
		switch name := r["__name__"]; name {
		case "ALERTS", "ALERTS_FOR_STATE", "target_info":
			// Shown by the alert columns, or not about the pod's own data.
		default:
			t.metrics = append(t.metrics, name)
		}
	}
	sort.Strings(t.metrics)

	res, err = o.query(ctx, fmt.Sprintf(`ALERTS{alertname=%q,%s}`, alert, sel))
	if err != nil {
		return t, err
	}
	for _, r := range res {
		if r["alertstate"] == "firing" || t.ruler == "" {
			t.ruler = r["alertstate"]
		}
	}

	q := url.Values{}
	q.Add("filter", fmt.Sprintf(`alertname=%q`, alert))
	q.Add("filter", fmt.Sprintf(`%s=%q`, o.namespaceLabel, namespace))
	q.Add("filter", fmt.Sprintf(`%s=%q`, o.podLabel, pod))
	var alerts []struct {
		Receivers []struct {
			Name string `json:"name"`
		} `json:"receivers"`
	}
	if err := getJSON(ctx, o.alertmanager+"/api/v2/alerts?"+q.Encode(), &alerts); err != nil {
		return t, fmt.Errorf("alertmanager: %w", err)
	}
	seen := map[string]bool{}
	for _, a := range alerts {
		for _, r := range a.Receivers {
			if !seen[r.Name] {
				seen[r.Name] = true
				t.receivers = append(t.receivers, r.Name)
			}
		}
	}
	sort.Strings(t.receivers)
	return t, nil
}

// query runs an instant query against Mimir's Prometheus API and returns each
// result's labels.
func (o options) query(ctx context.Context, promql string) ([]map[string]string, error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	u := o.mimir + "/prometheus/api/v1/query?" + url.Values{"query": {promql}}.Encode()
	if err := getJSON(ctx, u, &resp); err != nil {
		return nil, fmt.Errorf("mimir: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("mimir: %s: %s", promql, resp.Error)
	}
	var out []map[string]string
	for _, r := range resp.Data.Result {
		out = append(out, r.Metric)
	}
	return out, nil
}

func getJSON(ctx context.Context, u string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w (is Tilt's port-forward running?)", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 && len(body) == 0 {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: %s: %w", u, resp.Status, err)
	}
	return nil
}
