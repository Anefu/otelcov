// Package report turns a run's lineage into answers: for each requirement and
// pod, is there a path from the pod's signal to the alert and receiver, and if
// not, where does it break and why.
package report

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/otlptranslator"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/alerts"
	"github.com/Anefu/otelcov/internal/nodes/mimir"
	"github.com/Anefu/otelcov/internal/nodes/route"
	"github.com/Anefu/otelcov/internal/nodes/sdkenv"
	"github.com/Anefu/otelcov/internal/nodes/source"
	"github.com/Anefu/otelcov/internal/promqlx"
	"github.com/Anefu/otelcov/internal/requirements"
	"github.com/Anefu/otelcov/internal/topology"
)

// Statuses.
const (
	Covered          = "covered"
	CoveredSimulated = "covered_simulated"
	NotCovered       = "not_covered"
	Unverified       = "unverified"
)

// Entry is the answer for one requirement and pod.
type Entry struct {
	Requirement string             `json:"requirement"`
	Pod         string             `json:"pod"`
	Status      string             `json:"status"`
	Path        []lineage.SignalID `json:"path"`
	Receiver    string             `json:"receiver,omitempty"`
	BrokenAt    string             `json:"broken_at,omitempty"`
	// SimulatedBy lists the simulated events the path depends on.
	SimulatedBy []string `json:"simulated_by,omitempty"`
	// UnverifiedBy lists the unverified events on the path.
	UnverifiedBy []string `json:"unverified_by,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	// Destination names the alert and receiver (or other destination) the
	// status is about, when the requirement has several.
	Destination string `json:"destination,omitempty"`
}

// Diagnosis explains one break, from where it happened back to its root cause.
type Diagnosis struct {
	Requirement  string   `json:"requirement"`
	Pod          string   `json:"pod"`
	MissingLabel string   `json:"missing_label,omitempty"`
	Chain        []string `json:"chain"`
	RootCause    string   `json:"root_cause"`
	Explanation  string   `json:"explanation"`
	Hints        []string `json:"hints,omitempty"`
}

// Report is the result of a run.
type Report struct {
	Scenario  string                              `json:"scenario"`
	EvalTime  string                              `json:"eval_time"`
	Summary   []Entry                             `json:"summary"`
	Diagnosis []Diagnosis                         `json:"diagnosis"`
	Signals   map[lineage.SignalID]lineage.Signal `json:"signals"`
	Events    []lineage.Event                     `json:"events"`
	// Problems lists non-fatal problems found in the inputs: topology
	// validation, pod fields otelcov doesn't know.
	Problems []topology.Problem `json:"problems,omitempty"`
}

// Build answers the requirements from a run's lineage. pods are the pods of
// the config under test.
func Build(scenario, evalTime string, log *lineage.Log, reqs []requirements.Requirement, pods []corev1.Pod) *Report {
	ix := newIndex(log)
	r := &Report{
		Scenario: scenario, EvalTime: evalTime,
		Summary: []Entry{}, Diagnosis: []Diagnosis{},
		Signals: map[lineage.SignalID]lineage.Signal{}, Events: log.Events(),
	}
	for _, s := range log.Signals() {
		r.Signals[s.ID] = s
	}
	for _, req := range reqs {
		if req.Subjects.Kind != requirements.KindPod {
			r.Problems = append(r.Problems, topology.Problem{Severity: topology.Unverified, Where: "requirement " + req.ID,
				Message: fmt.Sprintf("subjects of kind %s aren't supported yet, so nothing was checked", req.Subjects.Kind)})
			continue
		}
		for _, pod := range req.Pods(pods) {
			entry, diag := ix.answerRequirement(req, pod)
			r.Summary = append(r.Summary, entry)
			if diag != nil {
				r.Diagnosis = append(r.Diagnosis, *diag)
			}
		}
	}
	return r
}

// check is one thing a pod's signal must do: reach an alert that is routed
// to a receiver ("" for any) and carries some labels.
type check struct {
	requirement string
	metrics     []string
	container   string
	alert       string
	receiver    string
	labels      []string
}

// answerRequirement checks every destination of a pod requirement and
// returns the answer that matters most: the first not covered, else the first
// unverified, else the first covered with simulation, else covered.
func (ix *index) answerRequirement(req requirements.Requirement, pod *corev1.Pod) (Entry, *Diagnosis) {
	base := Entry{Requirement: req.ID, Pod: kube.Ref(pod), Path: []lineage.SignalID{}}
	type result struct {
		e Entry
		d *Diagnosis
	}
	var results []result
	unsupported := func(dest, why string) {
		e := base
		e.Status, e.Reason, e.Destination = Unverified, why, dest
		results = append(results, result{e: e})
	}
	for _, dest := range req.Reaches {
		if dest.Alert == "" {
			kind := "backend"
			if dest.Dashboard != nil {
				kind = "dashboard"
			}
			unsupported(dest.String(), kind+" destinations aren't supported yet")
			continue
		}
		receivers := dest.Receivers
		if len(receivers) == 0 {
			receivers = []string{""}
		}
		for _, tmpl := range receivers {
			rcv, err := requirements.Resolve(tmpl, pod)
			if err != nil {
				unsupported(destination(dest.Alert, tmpl), err.Error())
				continue
			}
			e, d := ix.answer(check{requirement: req.ID, metrics: req.Signal.Metrics, container: req.Subjects.Container,
				alert: dest.Alert, receiver: rcv, labels: dest.Labels}, pod, base)
			e.Destination = destination(dest.Alert, rcv)
			results = append(results, result{e, d})
		}
	}
	for _, dest := range req.MustNotReach {
		unsupported("not "+dest.String(), "must_not_reach isn't supported yet")
	}
	if len(results) == 1 {
		results[0].e.Destination = ""
	}
	for _, status := range []string{NotCovered, Unverified, CoveredSimulated} {
		for _, r := range results {
			if r.e.Status == status {
				return r.e, r.d
			}
		}
	}
	return results[0].e, results[0].d
}

func destination(alert, receiver string) string {
	if receiver == "" {
		receiver = "any receiver"
	}
	return "alert " + alert + " → " + receiver
}

type index struct {
	signals  map[lineage.SignalID]lineage.Signal
	order    []lineage.SignalID
	children map[lineage.SignalID][]lineage.SignalID
	events   map[lineage.SignalID][]lineage.Event
}

func newIndex(log *lineage.Log) *index {
	ix := &index{
		signals:  map[lineage.SignalID]lineage.Signal{},
		children: map[lineage.SignalID][]lineage.SignalID{},
		events:   map[lineage.SignalID][]lineage.Event{},
	}
	for _, s := range log.Signals() {
		ix.signals[s.ID] = s
		ix.order = append(ix.order, s.ID)
		for _, p := range s.From {
			ix.children[p] = append(ix.children[p], s.ID)
		}
	}
	for _, e := range log.Events() {
		ix.events[e.Signal] = append(ix.events[e.Signal], e)
	}
	return ix
}

func (ix *index) find(id lineage.SignalID, match func(lineage.Event) bool) (lineage.Event, bool) {
	for _, e := range ix.events[id] {
		if match(e) {
			return e, true
		}
	}
	return lineage.Event{}, false
}

// sources finds the recorded signals of a check's metrics sent by pod.
func (ix *index) sources(c check, pod *corev1.Pod) []lineage.SignalID {
	ref := kube.Ref(pod)
	metric := map[string]bool{}
	for _, m := range c.metrics {
		metric[m] = true
	}
	var out []lineage.SignalID
	for _, id := range ix.order {
		s := ix.signals[id]
		if s.Node != source.Name || !metric[s.Name] {
			continue
		}
		if e, ok := ix.find(id, func(e lineage.Event) bool { return e.Node == sdkenv.Name && e.Action == lineage.Attributed }); ok {
			if e.Detail["pod"] == ref && (c.container == "" || e.Detail["container"] == c.container) {
				out = append(out, id)
			}
			continue
		}
		// Not attributed to a container: fall back to pod association, which
		// can't check the container.
		if _, ok := ix.find(id, func(e lineage.Event) bool {
			return e.Action == lineage.Associated && e.Detail["pod"] == ref
		}); ok && c.container == "" {
			out = append(out, id)
		}
	}
	return out
}

func (ix *index) answer(c check, pod *corev1.Pod, entry Entry) (Entry, *Diagnosis) {
	srcs := ix.sources(c, pod)
	if len(srcs) == 0 {
		entry.Status, entry.BrokenAt = Unverified, source.Name
		entry.Reason = "the recording has no " + strings.Join(c.metrics, " or ") + " from this pod"
		if c.container != "" {
			entry.Reason += "'s " + c.container + " container"
		}
		return entry, nil
	}
	var fallback *Entry
	var fallbackDiag *Diagnosis
	for _, s := range srcs {
		e, d := ix.answerFrom(c, s, entry)
		if e.Status == Covered || e.Status == CoveredSimulated {
			return e, nil
		}
		if fallback == nil {
			fallback, fallbackDiag = &e, d
		}
	}
	return *fallback, fallbackDiag
}

// answerFrom follows one source signal forward.
func (ix *index) answerFrom(c check, src lineage.SignalID, entry Entry) (Entry, *Diagnosis) {
	parent := map[lineage.SignalID]lineage.SignalID{}
	depth := map[lineage.SignalID]int{src: 0}
	queue := []lineage.SignalID{src}
	var visited []lineage.SignalID
	// hit: the alert delivered to the receiver. reached: the alert, delivered
	// elsewhere or nowhere.
	var hit, reached lineage.SignalID
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited = append(visited, id)
		if ix.signals[id].Node == alerts.Prefix+c.alert {
			if _, ok := ix.routedTo(id, c.receiver); ok {
				hit = id
				break
			}
			if reached == "" {
				reached = id
			}
		}
		for _, c := range ix.children[id] {
			if _, seen := depth[c]; !seen {
				depth[c], parent[c] = depth[id]+1, id
				queue = append(queue, c)
			}
		}
	}
	pathTo := func(end lineage.SignalID) []lineage.SignalID {
		var p []lineage.SignalID
		for id := end; ; id = parent[id] {
			p = append([]lineage.SignalID{id}, p...)
			if id == src {
				return p
			}
		}
	}

	if hit != "" {
		entry.Path = pathTo(hit)
		routed, _ := ix.routedTo(hit, c.receiver)
		entry.Receiver, _ = routed.Detail["receiver"].(string)
		if d := ix.missingLabels(c, hit, &entry); d != nil {
			return entry, d
		}
		needed := ix.neededAttributes(entry.Path, routed)
		for _, id := range entry.Path {
			for _, e := range ix.events[id] {
				relevant := e.Attribute == "" || needed[e.Attribute] || ix.signals[id].Node != source.Name
				if e.Action == lineage.Unverified && relevant {
					entry.UnverifiedBy = append(entry.UnverifiedBy, e.ID)
				}
				if e.Simulated && needed[e.Attribute] {
					entry.SimulatedBy = append(entry.SimulatedBy, e.ID)
				}
			}
		}
		switch {
		case len(entry.UnverifiedBy) > 0:
			entry.Status = Unverified
		case len(entry.SimulatedBy) > 0:
			entry.Status = CoveredSimulated
		default:
			entry.Status = Covered
		}
		return entry, nil
	}

	if reached != "" {
		// Reached the alert, but routing doesn't deliver it to the receiver.
		a := reached
		entry.Status, entry.BrokenAt, entry.Path = NotCovered, route.Name, pathTo(a)
		var chain []string
		var got []string
		for _, e := range ix.events[a] {
			if e.Node == route.Name {
				chain = append(chain, e.ID)
				if r, ok := e.Detail["receiver"].(string); ok {
					got = append(got, r)
				}
			}
		}
		d := &Diagnosis{Requirement: c.requirement, Pod: entry.Pod, Chain: chain,
			Explanation: fmt.Sprintf("%s fires for this pod, but routing sends it to %s, not %s.", q(c.alert), qs(got), q(c.receiver)),
			Hints:       []string{"Check the route matchers on the way to " + q(c.receiver) + " against the alert's labels."}}
		if c.receiver == "" {
			d.Explanation = fmt.Sprintf("%s fires for this pod, but no route delivers it to a receiver that sends notifications.", q(c.alert))
			d.Hints = []string{"Check the routes the alert matches, and that their receivers have integrations."}
		}
		if len(chain) > 0 {
			d.RootCause = chain[len(chain)-1]
		}
		return entry, d
	}

	// Where did it stop? The deepest data signal with a dropped event, else
	// the deepest with an unverified one. target_info series aren't the
	// requirement's signal.
	var stop lineage.SignalID
	var stopEvent lineage.Event
	for _, action := range []lineage.Action{lineage.Dropped, lineage.Unverified} {
		best := -1
		for _, id := range visited {
			if ix.signals[id].Labels["__name__"] == "target_info" {
				continue
			}
			if e, ok := ix.find(id, func(e lineage.Event) bool { return e.Action == action }); ok && depth[id] > best {
				best, stop, stopEvent = depth[id], id, e
			}
		}
		if stop != "" {
			break
		}
	}
	if stop == "" {
		entry.Status, entry.Path = Unverified, []lineage.SignalID{src}
		entry.Reason = "the signal reaches no alert and nothing explains why"
		return entry, nil
	}
	entry.Path, entry.BrokenAt = pathTo(stop), stopEvent.Node
	if stopEvent.Action == lineage.Unverified {
		entry.Status, entry.UnverifiedBy = Unverified, []string{stopEvent.ID}
		return entry, nil
	}
	entry.Status = NotCovered
	return entry, ix.diagnose(c, entry, stopEvent)
}

// missingLabels checks that an alert routed to the receiver carries the
// labels the check requires. If not, the entry is not covered at the alert.
func (ix *index) missingLabels(c check, alert lineage.SignalID, entry *Entry) *Diagnosis {
	have := ix.signals[alert].Labels
	var missing []string
	for _, l := range c.labels {
		if have[l] == "" {
			missing = append(missing, l)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	entry.Status, entry.BrokenAt = NotCovered, alerts.Prefix+c.alert
	d := &Diagnosis{Requirement: c.requirement, Pod: entry.Pod, MissingLabel: missing[0],
		Explanation: fmt.Sprintf("%s fires for this pod and reaches %s, but it has no %s label, which the requirement says it must carry.", q(c.alert), q(entry.Receiver), qs(missing)),
		Hints:       []string{fmt.Sprintf("Keep %s in the alerting rule's expression (for example in its `by (…)`), or set it in the rule's labels.", qs(missing))}}
	if e, ok := ix.find(alert, func(e lineage.Event) bool { return e.Action == lineage.Derived }); ok {
		d.Chain, d.RootCause = []string{e.ID}, e.ID
	}
	return d
}

// routedTo finds the routing event delivering an alert to receiver (any
// receiver when empty).
func (ix *index) routedTo(alert lineage.SignalID, receiver string) (lineage.Event, bool) {
	return ix.find(alert, func(e lineage.Event) bool {
		return e.Node == route.Name && e.Action == lineage.Routed && (receiver == "" || e.Detail["receiver"] == receiver)
	})
}

// neededAttributes returns the resource attributes the path depends on: those
// promoted to labels that a selector or route matcher on the path requires.
func (ix *index) neededAttributes(path []lineage.SignalID, routed lineage.Event) map[string]bool {
	neededLabels := map[string]bool{}
	for _, id := range path {
		for _, e := range ix.events[id] {
			if e.Action != lineage.Derived {
				continue
			}
			expr, _ := e.Detail["expr"].(string)
			if expr == "" {
				continue
			}
			parsed, err := parser.ParseExpr(expr)
			if err != nil {
				continue
			}
			for _, vs := range promqlx.Selectors(parsed) {
				for _, m := range vs.LabelMatchers {
					if m.Name != labels.MetricName && !m.Matches("") {
						neededLabels[m.Name] = true
					}
				}
			}
		}
	}
	if steps, ok := routed.Detail["route"].([]any); ok {
		for _, st := range steps {
			ms, _ := st.(map[string]any)["matchers"].([]any)
			for _, m := range ms {
				name := strings.TrimLeft(strings.SplitN(m.(string), "=", 2)[0], " ")
				name = strings.TrimRight(name, "!~")
				neededLabels[name] = true
			}
		}
	}
	out := map[string]bool{}
	for _, id := range path {
		e, ok := ix.find(id, func(e lineage.Event) bool {
			return strings.HasPrefix(e.Node, mimir.Prefix) && e.Action == lineage.Derived
		})
		if !ok {
			continue
		}
		if promoted, ok := e.Detail["promoted"].(map[string]any); ok {
			for attr, lbl := range promoted {
				if neededLabels[lbl.(string)] {
					out[attr] = true
				}
			}
		}
		if neededLabels["job"] {
			out["service.name"], out["service.namespace"] = true, true
		}
		if neededLabels["instance"] {
			out["service.instance.id"] = true
		}
	}
	return out
}

// diagnose traces a drop back to its root cause.
func (ix *index) diagnose(c check, entry Entry, drop lineage.Event) *Diagnosis {
	d := &Diagnosis{Requirement: c.requirement, Pod: entry.Pod, Chain: []string{drop.ID}, RootCause: drop.ID}
	label := drop.Attribute
	matcher := ""
	if reqs, ok := drop.Detail["requires"].(map[string]any); ok {
		matcher, _ = reqs["matcher"].(string)
	}
	where := nodeRef(drop.Node)
	if label == "" {
		d.Explanation = fmt.Sprintf("%s drops this pod's series: %s.", capitalize(where), drop.Reason)
		d.Hints = []string{"Check the matching in " + where + " against this pod's series."}
		return d
	}
	d.MissingLabel = label
	d.Explanation = fmt.Sprintf("%s only selects series matching %s, and this pod's series has no %s label, so it never reaches %s.",
		capitalize(where), q(matcher), q(label), q(c.alert))

	// Which resource attribute should have become the label?
	namer := otlptranslator.LabelNamer{}
	var attr string
	var backend lineage.Event
	for i := len(entry.Path) - 1; i >= 0 && attr == ""; i-- {
		for _, e := range ix.events[entry.Path[i]] {
			if strings.HasPrefix(e.Node, mimir.Prefix) && e.Action == lineage.Skipped && namer.Build(e.Attribute) == label {
				attr, backend = e.Attribute, e
				break
			}
		}
	}
	if attr == "" {
		d.Hints = []string{fmt.Sprintf("No resource attribute is promoted to %s. Promote one in Mimir, or relax %s in %s.", q(label), q(matcher), where)}
		return d
	}
	d.Chain = append(d.Chain, backend.ID)
	d.RootCause = backend.ID
	d.Explanation += fmt.Sprintf(" %s comes from resource attribute %s", q(label), q(attr))

	// The last node that touched the attribute on the recorded signal.
	var root lineage.Event
	for _, e := range ix.events[entry.Path[0]] {
		if e.Attribute == attr && (e.Action == lineage.Skipped || e.Action == lineage.Removed || e.Action == lineage.Unverified) {
			root = e
		}
	}
	if root.ID == "" {
		d.Explanation += ", which nothing sets for this pod."
		d.Hints = []string{fmt.Sprintf("Set %s before the data reaches the backend: in the SDK's `OTEL_RESOURCE_ATTRIBUTES`, or with a collector processor.", q(attr))}
		return d
	}
	d.Chain = append(d.Chain, root.ID)
	d.RootCause = root.ID
	d.Explanation += fmt.Sprintf(", which %s didn't set: %s.", q(root.Node), root.Reason)
	d.Hints = hints(c, attr, root, matcher, where)
	return d
}

func hints(chk check, attr string, root lineage.Event, matcher, where string) []string {
	c := chk.container
	switch {
	case strings.HasSuffix(root.Node, "k8sattributes") && attr == "k8s.container.name" && c != "":
		return []string{
			fmt.Sprintf("Set %s in %s on the %s container.", q("k8s.container.name="+c), q("OTEL_RESOURCE_ATTRIBUTES"), q(c)),
			fmt.Sprintf("Or have the SDK send %s, so k8sattributes can match the container.", q("container.id")),
		}
	case root.Action == lineage.Skipped && root.Detail["statement"] != nil:
		// The explanation already quotes the statement; the hint says what to change.
		if cond, ok := root.Detail["where"].(string); ok && cond != "" {
			return []string{fmt.Sprintf("Make the condition %s in %s hold for this pod, or set %s another way.", q(cond), q(root.Node), q(attr))}
		}
		return []string{fmt.Sprintf("Make the statement's condition in %s hold for this pod, or set %s another way.", q(root.Node), q(attr))}
	case root.Node == sdkenv.Name && root.Action == lineage.Removed:
		return []string{fmt.Sprintf("The change under test removes %s from the SDK environment. Keep it, or relax %s in %s.", q(attr), q(matcher), where)}
	}
	return []string{fmt.Sprintf("Make sure the series carries %s, or relax %s in %s.", q(attr), q(matcher), where)}
}

// Explanations and hints quote identifiers in backticks; the report page and
// the CLI show quoted spans as code.
func q(s string) string { return "`" + s + "`" }

func qs(list []string) string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = q(s)
	}
	return strings.Join(out, ", ")
}

// nodeRef names a lineage node in prose: "rule/x" is rule `x`.
func nodeRef(node string) string {
	if kind, name, ok := strings.Cut(node, "/"); ok && (kind == "rule" || kind == "alert") {
		return kind + " " + q(name)
	}
	return q(node)
}

func capitalize(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// SortedSignalIDs returns signal IDs in a stable, readable order.
func SortedSignalIDs(m map[lineage.SignalID]lineage.Signal) []lineage.SignalID {
	ids := make([]lineage.SignalID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := string(ids[i]), string(ids[j])
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		na, _ := strconv.Atoi(a[1:])
		nb, _ := strconv.Atoi(b[1:])
		return na < nb
	})
	return ids
}
