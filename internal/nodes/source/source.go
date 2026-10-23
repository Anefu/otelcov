// Package source replays recorded telemetry: OTLP/JSON, one export request per
// line, as the collector's file exporter writes it.
package source

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// Name is the node name used in lineage.
const Name = "source/otlp-replay"

// maxLine bounds one export request; the file exporter writes one per line.
const maxLine = 64 << 20

// Replay reads a metrics recording and splits it into items, one per data
// point, with IDs s1, s2, ... in file order.
type Replay struct {
	path string
	// display is the path shown in signal origins.
	display string
}

// New returns a source for the recording at path. display is how the file is
// named in reports.
func New(path, display string) *Replay {
	return &Replay{path: path, display: display}
}

func (r *Replay) Name() string { return Name }

func (r *Replay) ReadMetrics(ctx context.Context) (graph.Output, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return graph.Output{}, err
	}
	defer f.Close()

	var (
		out  graph.Output
		u    pmetric.JSONUnmarshaler
		line int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	for sc.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return graph.Output{}, err
		}
		if len(sc.Bytes()) == 0 {
			continue
		}
		md, err := u.UnmarshalMetrics(sc.Bytes())
		if err != nil {
			return graph.Output{}, fmt.Errorf("%s:%d: %w", r.display, line, err)
		}
		for _, part := range unit.SplitMetrics(md) {
			id := lineage.SignalID("s" + strconv.Itoa(len(out.Items)+1))
			out.Items = append(out.Items, unit.Metric{ID: id, Data: part})
			out.Signals = append(out.Signals, lineage.Signal{
				ID:       id,
				Node:     Name,
				Kind:     "otlp_metric",
				Name:     unit.FirstMetric(part).Name(),
				Origin:   r.display + "#L" + strconv.Itoa(line),
				Resource: unit.Resource(part).Attributes().AsRaw(),
			})
		}
	}
	if err := sc.Err(); err != nil {
		return graph.Output{}, fmt.Errorf("%s: %w", r.display, err)
	}
	return out, nil
}
