// Command otelcov-plugin-otelcol-contrib is the stage plugin for the
// otelcol-contrib collector distribution (internal/plugins/otelcolcontrib).
// otelcov starts it; it isn't meant to be run by hand.
package main

import (
	"github.com/Anefu/otelcov/internal/plugins/otelcolcontrib"
	"github.com/Anefu/otelcov/pkg/plugin"
)

func main() { plugin.Serve(otelcolcontrib.New()) }
