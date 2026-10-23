module github.com/Anefu/otelcov

go 1.24

// Pinned to the collector version the checked collectors run (0.135.x).
// pdata v1.41.0 ships with collector core and contrib v0.135.0.
require go.opentelemetry.io/collector/pdata v1.41.0
