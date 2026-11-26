package topology

import (
	"testing"

	"github.com/Anefu/otelcov/internal/plugintest"
)

// Runs go through the stage plugins, built once for this package's tests.
func TestMain(m *testing.M) { plugintest.Main(m) }
