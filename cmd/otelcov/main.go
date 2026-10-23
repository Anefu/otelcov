// Command otelcov checks OpenTelemetry monitoring coverage.
//
//	otelcov scenario [-root dir] [-json] [-html report.html] scenario.yaml...
//
// It runs each scenario and prints, per requirement and pod, whether the pod's
// signal reaches its alert and receiver, and why not when it doesn't. With
// -html it also writes one page showing every scenario: a coverage matrix and
// each pod's route through the stack. The exit status is 1 when anything is
// not covered, 2 on errors.
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
	"github.com/Anefu/otelcov/internal/report"
	"github.com/Anefu/otelcov/internal/scenario"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "scenario" {
		fmt.Fprintln(stderr, "usage: otelcov scenario [-root dir] [-json] [-html report.html] scenario.yaml...")
		return 2
	}
	fs := flag.NewFlagSet("scenario", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "directory report paths are relative to (default: the scenario's parent directory)")
	asJSON := fs.Bool("json", false, "print full reports as JSON")
	htmlOut := fs.String("html", "", "also write a report page for all scenarios to this file")
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
			if e.Status == report.NotCovered {
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
			report.Covered:          "covered",
			report.CoveredSimulated: "covered (simulated)",
			report.NotCovered:       "NOT COVERED",
			report.Unverified:       "unverified",
		}[e.Status]
		fmt.Fprintf(w, "  %-20s %s  [%s]\n", label, e.Pod, e.Requirement)
		path := make([]string, len(e.Path))
		for i, id := range e.Path {
			path[i] = string(id)
		}
		switch {
		case e.Receiver != "":
			fmt.Fprintf(w, "      path: %s → %s\n", strings.Join(path, " → "), e.Receiver)
		case len(path) > 0:
			fmt.Fprintf(w, "      path: %s ✗ %s\n", strings.Join(path, " → "), e.BrokenAt)
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
