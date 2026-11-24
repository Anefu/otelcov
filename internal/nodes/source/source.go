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

// File is one recorded file.
type File struct {
	Path string
	// Display is how the file is named in signal origins.
	Display string
}

// Replay reads metrics recordings, file by file, and splits them into items,
// one per data point, with IDs s1, s2, ... in order.
type Replay struct {
	files []File
	// FileOf says which file (an index into the files given to New) each
	// item came from, once ReadMetrics has run.
	FileOf map[lineage.SignalID]int
}

// New returns a source for the recordings, replayed in the order given.
func New(files ...File) *Replay {
	return &Replay{files: files, FileOf: map[lineage.SignalID]int{}}
}

func (r *Replay) Name() string { return Name }

func (r *Replay) ReadMetrics(ctx context.Context) (graph.Output, error) {
	var out graph.Output
	for i, f := range r.files {
		if err := r.read(ctx, i, f, &out); err != nil {
			return graph.Output{}, err
		}
	}
	return out, nil
}

func (r *Replay) read(ctx context.Context, index int, file File, out *graph.Output) error {
	f, err := os.Open(file.Path)
	if err != nil {
		return err
	}
	defer f.Close()

	var (
		u    pmetric.JSONUnmarshaler
		line int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	for sc.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(sc.Bytes()) == 0 {
			continue
		}
		md, err := u.UnmarshalMetrics(sc.Bytes())
		if err != nil {
			return fmt.Errorf("%s:%d: %w", file.Display, line, err)
		}
		for _, part := range unit.SplitMetrics(md) {
			id := lineage.SignalID("s" + strconv.Itoa(len(out.Items)+1))
			r.FileOf[id] = index
			out.Items = append(out.Items, unit.Metric{ID: id, Data: part})
			out.Signals = append(out.Signals, lineage.Signal{
				ID:       id,
				Node:     Name,
				Kind:     "otlp_metric",
				Name:     unit.FirstMetric(part).Name(),
				Origin:   file.Display + "#L" + strconv.Itoa(line),
				Resource: unit.Resource(part).Attributes().AsRaw(),
			})
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", file.Display, err)
	}
	return nil
}
