#!/usr/bin/env bash
# CI step: Unit, conformance and cross-process recovery tests
set -e
root=$(git rev-parse --show-toplevel)
bin=$root/.ci/bin
JPACK_TEST_BIN="$bin/jpack" \
JPACK_EARLIER_TEST_BIN="$bin/jpack-earlier" \
JPACK_GATEWAY_TEST_BIN="$bin/gateway" \
JPACK_MCP_ADAPTER_TEST_BIN="$bin/adapter-mcp" \
JPACK_OLD_RUNNER_TEST_BIN="$bin/jpack-runner-old" \
go test -race ./...
