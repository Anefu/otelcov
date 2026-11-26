// Package plugintest builds the stage plugins for tests that run scenarios.
package plugintest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Anefu/otelcov/internal/pluginhost"
)

// Plugins are the stage plugin commands in this repository.
var Plugins = []string{"github.com/Anefu/otelcov/cmd/otelcov-plugin-otelcol-contrib"}

// Build compiles the stage plugins into dir.
func Build(dir string) error {
	for _, pkg := range Plugins {
		out := filepath.Join(dir, filepath.Base(pkg))
		cmd := exec.Command("go", "build", "-o", out, pkg)
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("building %s: %w\n%s", pkg, err, b)
		}
	}
	return nil
}

// Main builds the plugins into a temporary directory, points
// $OTELCOV_PLUGINS at it, and runs the tests. Use it as TestMain.
func Main(m *testing.M) {
	dir, err := os.MkdirTemp("", "otelcov-plugins-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := Build(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.RemoveAll(dir)
		os.Exit(2)
	}
	os.Setenv(pluginhost.EnvDir, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
