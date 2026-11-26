#!/bin/sh
# Regenerates the Go code for proto/ (pkg/plugin/pluginv1). Needs protoc on
# PATH (generated with libprotoc 28.3); the Go generators are the versions
# pinned in go.mod's tool directives.
set -eu
cd "$(dirname "$0")/.."
bin=$(mktemp -d)
trap 'rm -rf "$bin"' EXIT
go build -o "$bin/protoc-gen-go" google.golang.org/protobuf/cmd/protoc-gen-go
go build -o "$bin/protoc-gen-go-grpc" google.golang.org/grpc/cmd/protoc-gen-go-grpc
protoc -I proto \
  --plugin=protoc-gen-go="$bin/protoc-gen-go" --go_out=. --go_opt=module=github.com/Anefu/otelcov \
  --plugin=protoc-gen-go-grpc="$bin/protoc-gen-go-grpc" --go-grpc_out=. --go-grpc_opt=module=github.com/Anefu/otelcov \
  proto/otelcov/plugin/v1/*.proto
