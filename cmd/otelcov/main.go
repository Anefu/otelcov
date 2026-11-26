// Command otelcov checks OpenTelemetry monitoring coverage.
//
//	otelcov scenario [-root dir] [-json] [-html report.html] scenario.yaml...
//	otelcov version
//
// It runs each scenario and prints, per requirement and pod, whether the pod's
// signal reaches its alert and receiver, and why not when it doesn't. With
// -html it also writes one page showing every scenario: a coverage matrix and
// each pod's route through the stack. The exit status is 1 when anything is
// not covered (by the components' behavior or by otelcov's models), 2 on
// errors. otelcov version prints otelcov's version and the stage plugins it
// finds.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Anefu/otelcov/internal/htmlreport"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/pluginhost"
	"github.com/Anefu/otelcov/internal/report"
	"github.com/Anefu/otelcov/internal/scenario"
	"github.com/Anefu/otelcov/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
  otelcov scenario [-root dir] [-plugins dir] [-json] [-html report.html] scenario.yaml...
  otelcov version [-plugins dir]

Stage plugins (otelcov-plugin-*) are found in -plugins, else $OTELCOV_PLUGINS,
else next to the otelcov binary.`

const pluginsHelp = "directory of stage plugins (default: $OTELCOV_PLUGINS, else next to the otelcov binary)"

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 && args[0] == "version" {
		fs := flag.NewFlagSet("version", flag.ContinueOnError)
		fs.SetOutput(stderr)
		plugins := fs.String("plugins", "", pluginsHelp)
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		fmt.Fprintf(stdout, "otelcov %s\n", version.Get().Otelcov)
		host := pluginhost.New(*plugins, pluginhost.Inputs{})
		defer host.Close()
		installed, err := host.Installed()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if len(installed) == 0 {
			fmt.Fprintf(stdout, "no stage plugins in %s\n", strings.Join(host.Dirs(), ", "))
		}
		for _, p := range installed {
			fmt.Fprintf(stdout, "plugin %s: models %s %s, reviewed for %s (collector-contrib %s, pdata %s)\n",
				filepath.Base(p.Path), p.Distribution, p.Version, p.Reviewed, p.Modules["collector-contrib"], p.Modules["pdata"])
		}
		return 0
	}
	if len(args) == 0 || args[0] != "scenario" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("scenario", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "directory report paths are relative to (default: the scenario's parent directory)")
	asJSON := fs.Bool("json", false, "print full reports as JSON")
	htmlOut := fs.String("html", "", "also write a report page for all scenarios to this file")
	plugins := fs.String("plugins", "", pluginsHelp)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "no scenario files given")
		return 2
	}

	status := 0
	page := htmlreport.Data{Generated: time.Now().UTC().Format(time.RFC3339)}
	for _, path := range fs.Args() {
		r := *root
		if r == "" {
			r = filepath.Dir(filepath.Dir(path))
		}
		s, err := scenario.Load(path, r)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		s.PluginDir = *plugins
		rep, flow, err := scenario.ReportWithFlow(context.Background(), s)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", path, err)
			return 2
		}
		page.Scenarios = append(page.Scenarios, htmlreport.Scenario{Description: s.Description, Report: rep, Flow: flow})
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(rep); err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
		} else {
			printText(stdout, rep)
		}
		for _, e := range rep.Summary {
			if report.Failed(e.Status) {
				status = 1
			}
		}
	}
	if *htmlOut != "" {
		if err := writePage(*htmlOut, page); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintf(stderr, "wrote %s\n", *htmlOut)
	}
	return status
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

func printText(w io.Writer, rep *report.Report) {
	fmt.Fprintf(w, "%s (evaluated at %s)\n", rep.Scenario, rep.EvalTime)
	for _, p := range rep.Problems {
		fmt.Fprintf(w, "  %s: %s: %s\n", p.Severity, p.Where, p.Message)
	}
	diag := map[string]report.Diagnosis{}
	for _, d := range rep.Diagnosis {
		diag[d.Requirement+"|"+d.Pod] = d
	}
	events := map[string]lineage.Event{}
	for _, e := range rep.Events {
		events[e.ID] = e
	}
	for _, e := range rep.Summary {
		label := map[string]string{
			report.Covered:            "covered",
			report.CoveredModelled:    "covered (modelled)",
			report.NotCovered:         "NOT COVERED",
			report.NotCoveredModelled: "NOT COVERED (modelled)",
			report.Unverified:         "unverified",
		}[e.Status]
		about := e.Requirement
		if e.Destination != "" {
			about += ": " + e.Destination
		}
		fmt.Fprintf(w, "  %-23s %s  [%s]\n", label, e.Pod, about)
		path := make([]string, len(e.Path))
		for i, id := range e.Path {
			path[i] = string(id)
		}
		switch {
		case len(path) > 0 && e.BrokenAt != "":
			fmt.Fprintf(w, "      path: %s ✗ %s\n", strings.Join(path, " → "), e.BrokenAt)
		case e.Receiver != "":
			fmt.Fprintf(w, "      path: %s → %s\n", strings.Join(path, " → "), e.Receiver)
		case len(path) > 0:
			fmt.Fprintf(w, "      path: %s\n", strings.Join(path, " → "))
		}
		if len(e.ModelledBy) > 0 {
			fmt.Fprintf(w, "      modelled: %s\n", modelled(e.ModelledBy, rep.Stages))
		}
		for _, id := range e.SimulatedBy {
			ev := events[id]
			fmt.Fprintf(w, "      depends on simulated %s=%v (%s)\n", ev.Attribute, ev.Value, ev.Node)
		}
		for _, id := range e.UnverifiedBy {
			fmt.Fprintf(w, "      unverified: %s: %s\n", events[id].Node, events[id].Reason)
		}
		if e.Reason != "" {
			fmt.Fprintf(w, "      %s\n", e.Reason)
		}
		if d, ok := diag[e.Requirement+"|"+e.Pod]; ok {
			fmt.Fprintf(w, "      why: %s\n", d.Explanation)
			for _, h := range d.Hints {
				fmt.Fprintf(w, "      fix: %s\n", h)
			}
		}
	}
}

// modelled lists modelled stages, grouped by what they are:
// "agent/batch, gateway/batch (approximation: passes data through); …".
func modelled(nodes []string, stages map[string]lineage.Stage) string {
	var order []string
	by := map[string][]string{}
	for _, n := range nodes {
		st := stages[n]
		what := string(st.Fidelity)
		switch {
		case st.Fidelity == lineage.Reimplementation:
			what = "otelcov's own model, not tested against the real component"
		case st.Note != "":
			what += ": " + st.Note
		}
		if _, ok := by[what]; !ok {
			order = append(order, what)
		}
		by[what] = append(by[what], n)
	}
	parts := make([]string, len(order))
	for i, what := range order {
		parts[i] = strings.Join(by[what], ", ") + " (" + what + ")"
	}
	return strings.Join(parts, "; ")
}
