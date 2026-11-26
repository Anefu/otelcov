// Package pluginhost finds, starts and talks to stage plugins for one run
// (decisions 67 to 69). A plugin models one distribution at one version and
// declares the deployed versions it has been reviewed against; a collector
// whose version falls outside every installed plugin's range gets no plugin,
// and its stages are unverified.
package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/pkg/plugin"
	"github.com/Anefu/otelcov/pkg/plugin/pluginv1"
)

// Prefix starts every plugin binary's file name.
const Prefix = "otelcov-plugin-"

// EnvDir names the environment variable with the plugin directories
// (separated like PATH) used when no directory is given.
const EnvDir = "OTELCOV_PLUGINS"

// Inputs are what Init gives every plugin of a run.
type Inputs struct {
	Cluster      kube.Objects
	SnapshotPods []corev1.Pod
	// ResourceDetection is the snapshot metadata's resource_detection.
	ResourceDetection any
}

// Info describes a plugin, for reports.
type Info struct {
	Distribution string            `json:"distribution"`
	Version      string            `json:"version"`
	Reviewed     string            `json:"reviewed"`
	Modules      map[string]string `json:"modules,omitempty"`
	Path         string            `json:"-"`
	// Processors are the processor types the plugin models.
	Processors map[string]Component `json:"-"`
}

// Component is how a plugin models a component type.
type Component struct {
	Fidelity pluginv1.Fidelity
	Note     string
}

// ProcessorNote is a flow graph's note for a processor type: the plugin's
// note, or, for a type it doesn't model, that results after it are
// unverified.
func (i Info) ProcessorNote(typ string) string {
	c, ok := i.Processors[typ]
	if !ok || c.Fidelity == pluginv1.Fidelity_FIDELITY_UNMODELLED {
		return NotModeled
	}
	return c.Note
}

// NotModeled notes a component without a model in flow graphs.
const NotModeled = "not modeled: results after it are unverified"

func (i Info) String() string {
	return fmt.Sprintf("%s %s (reviewed for %s)", i.Distribution, i.Version, i.Reviewed)
}

type candidate struct {
	info     Info
	min, max [2]int
	conn     *plugin.Conn
	started  bool
	plugin   *Plugin
}

// Host is the plugins of one run.
type Host struct {
	dirs       []string
	in         Inputs
	candidates []*candidate
	discovered bool
	err        error
}

// New returns a host that looks for plugins in dir: a list separated like
// PATH, or, when empty, $OTELCOV_PLUGINS, else the directory of the running
// executable.
func New(dir string, in Inputs) *Host {
	if dir == "" {
		dir = os.Getenv(EnvDir)
	}
	if dir == "" {
		if exe, err := os.Executable(); err == nil {
			dir = filepath.Dir(exe)
		}
	}
	return &Host{dirs: filepath.SplitList(dir), in: in}
}

// discover starts every plugin binary once to ask what it models. Plugins
// that aren't used are stopped again by Resolve's callers via Close.
func (h *Host) discover() error {
	if h.discovered {
		return h.err
	}
	h.discovered = true
	for _, d := range h.dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), Prefix) {
				continue
			}
			path := filepath.Join(d, e.Name())
			if st, err := os.Stat(path); err != nil || st.Mode()&0o111 == 0 {
				continue
			}
			conn, err := plugin.Start(path)
			if err != nil {
				h.err = err
				return err
			}
			desc, err := conn.Describe(context.Background(), &pluginv1.DescribeRequest{})
			if err != nil {
				conn.Stop()
				h.err = fmt.Errorf("plugin %s: %w", path, err)
				return h.err
			}
			c := &candidate{conn: conn, info: Info{
				Distribution: desc.GetDistribution(), Version: desc.GetVersion(), Modules: desc.GetModules(), Path: path,
				Reviewed:   desc.GetReviewed().GetMin() + "–" + desc.GetReviewed().GetMax(),
				Processors: map[string]Component{},
			}}
			for _, comp := range desc.GetComponents() {
				if comp.GetKind() == "processor" {
					c.info.Processors[comp.GetType()] = Component{Fidelity: comp.GetFidelity(), Note: comp.GetNote()}
				}
			}
			var ok1, ok2 bool
			c.min, ok1 = minor(desc.GetReviewed().GetMin())
			c.max, ok2 = minor(desc.GetReviewed().GetMax())
			if !ok1 || !ok2 {
				conn.Stop()
				h.err = fmt.Errorf("plugin %s: reviewed range %s isn't two versions", path, c.info.Reviewed)
				return h.err
			}
			h.candidates = append(h.candidates, c)
		}
	}
	return nil
}

// Plugin is a plugin chosen for a run.
type Plugin struct{ c *candidate }

// Info describes the plugin.
func (p *Plugin) Info() Info { return p.c.info }

// Resolve picks the plugin for a distribution at a deployed version: the
// installed plugin for that distribution whose reviewed range includes the
// version, preferring one built for the same minor version, then the newest.
// When none fits, why says so for reports.
func (h *Host) Resolve(distribution, version string) (p *Plugin, why string, err error) {
	if err := h.discover(); err != nil {
		return nil, "", err
	}
	v, ok := minor(version)
	if !ok {
		return nil, fmt.Sprintf("version %q isn't a version like 0.139.0", version), nil
	}
	var best *candidate
	var same []string
	for _, c := range h.candidates {
		if c.info.Distribution != distribution {
			continue
		}
		same = append(same, c.info.String())
		if less(v, c.min) || less(c.max, v) {
			continue
		}
		if best == nil || better(c, best, v) {
			best = c
		}
	}
	if best != nil {
		if best.plugin == nil {
			best.plugin = &Plugin{c: best}
		}
		return best.plugin, "", nil
	}
	switch {
	case len(h.candidates) == 0:
		return nil, fmt.Sprintf("no stage plugins are installed (looked in %s)", strings.Join(h.dirs, ", ")), nil
	case len(same) == 0:
		var all []string
		for _, c := range h.candidates {
			all = append(all, c.info.String())
		}
		sort.Strings(all)
		return nil, fmt.Sprintf("no plugin models distribution %s; installed: %s", distribution, strings.Join(all, ", ")), nil
	}
	sort.Strings(same)
	return nil, fmt.Sprintf("no plugin was reviewed for %s %s; installed: %s", distribution, version, strings.Join(same, ", ")), nil
}

// Start calls Init on the plugin with the run's inputs and the feature gates
// of every collector it models. Call it once per plugin, before Stage.
func (h *Host) Start(ctx context.Context, p *Plugin, gates []string) error {
	if p.c.started {
		return fmt.Errorf("plugin %s started twice", p.c.info)
	}
	p.c.started = true
	req := &pluginv1.InitRequest{FeatureGates: gates}
	var err error
	if req.Cluster, err = kube.Marshal(h.in.Cluster); err != nil {
		return err
	}
	if req.SnapshotPods, err = kube.Marshal(kube.Objects{Pods: h.in.SnapshotPods}); err != nil {
		return err
	}
	if h.in.ResourceDetection != nil {
		if req.ResourceDetection, err = json.Marshal(h.in.ResourceDetection); err != nil {
			return err
		}
	}
	if _, err := p.c.conn.Init(ctx, req); err != nil {
		return fmt.Errorf("plugin %s: %w", p.c.info, err)
	}
	return nil
}

// Installed describes every plugin found.
func (h *Host) Installed() ([]Info, error) {
	if err := h.discover(); err != nil {
		return nil, err
	}
	out := make([]Info, len(h.candidates))
	for i, c := range h.candidates {
		out[i] = c.info
	}
	return out, nil
}

// Dirs are the directories the host looks in.
func (h *Host) Dirs() []string { return h.dirs }

// Used describes the plugins started in this run.
func (h *Host) Used() []Info {
	var out []Info
	for _, c := range h.candidates {
		if c.started {
			out = append(out, c.info)
		}
	}
	return out
}

// Close stops every plugin process.
func (h *Host) Close() {
	for _, c := range h.candidates {
		c.conn.Stop()
	}
	h.candidates = nil
}

// minor parses the major and minor version of "0.139", "0.139.1" or
// "v0.139.1".
func minor(v string) ([2]int, bool) {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return [2]int{}, false
	}
	var out [2]int
	for i := range out {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return [2]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func less(a, b [2]int) bool { return a[0] < b[0] || a[0] == b[0] && a[1] < b[1] }

// better prefers a plugin built for the deployed minor version, then the
// newest build.
func better(a, b *candidate, deployed [2]int) bool {
	av, _ := minor(a.info.Version)
	bv, _ := minor(b.info.Version)
	if (av == deployed) != (bv == deployed) {
		return av == deployed
	}
	return less(bv, av)
}
