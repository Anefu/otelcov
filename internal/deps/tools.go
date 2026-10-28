//go:build tools

// Package deps pins packages that must be vendored before any code imports
// them. The vendor workflow only vendors imported packages, so listing them
// here lets development builds use them without another vendoring round trip.
package deps

import (
	_ "github.com/distribution/reference"
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

	_ "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlmetric"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlresource"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlscope"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/processor/attributesprocessor"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/processor/deltatocumulativeprocessor"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/processor/metricstransformprocessor"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourceprocessor"
	_ "github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor"
	_ "go.opentelemetry.io/collector/component"
	_ "go.opentelemetry.io/collector/component/componenttest"
	_ "go.opentelemetry.io/collector/confmap"
	_ "go.opentelemetry.io/collector/confmap/xconfmap"
	_ "go.opentelemetry.io/collector/consumer"
	_ "go.opentelemetry.io/collector/consumer/consumertest"
	_ "go.opentelemetry.io/collector/featuregate"
	_ "go.opentelemetry.io/collector/pipeline"
	_ "go.opentelemetry.io/collector/processor"
	_ "go.opentelemetry.io/collector/processor/processortest"
)
