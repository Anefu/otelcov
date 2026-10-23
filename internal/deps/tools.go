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

	_ "github.com/prometheus/alertmanager/config"
	_ "github.com/prometheus/alertmanager/dispatch"
	_ "github.com/prometheus/common/model"
	_ "github.com/prometheus/prometheus/model/labels"
	_ "github.com/prometheus/prometheus/model/rulefmt"
	_ "github.com/prometheus/prometheus/prompb"
	_ "github.com/prometheus/prometheus/promql"
	_ "github.com/prometheus/prometheus/promql/parser"
	_ "github.com/prometheus/prometheus/storage"
	_ "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
	_ "github.com/prometheus/prometheus/util/annotations"
)
