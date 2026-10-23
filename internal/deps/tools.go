//go:build tools

// Package deps pins packages that must be vendored before any code imports
// them. The vendor workflow only vendors imported packages, so listing them
// here lets development builds use them without another vendoring round trip.
package deps

import (
	_ "go.opentelemetry.io/collector/pdata/pcommon"
	_ "go.opentelemetry.io/collector/pdata/plog"
	_ "go.opentelemetry.io/collector/pdata/pmetric"
	_ "go.opentelemetry.io/collector/pdata/ptrace"
	_ "k8s.io/api/apps/v1"
	_ "k8s.io/api/core/v1"
	_ "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "sigs.k8s.io/yaml"
)
