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
# The store contract's conformance suite runs against every kind
# (docs/design/store-interface.md): a kind whose cases do not run and pass
# fails this step, as a skipped test would otherwise pass.
out="$(go test -race -count=1 -v -run '^TestStoreKindsConform$' ./internal/runner)" || { printf '%s\n' "$out"; exit 1; }
for kind in sqlite memory; do
  printf '%s\n' "$out" | grep -q -- "^[[:space:]]*--- PASS: TestStoreKindsConform/$kind " || { echo "TestStoreKindsConform/$kind did not run and pass"; exit 1; }
done
echo "store conformance: the sqlite and memory kinds ran and passed"
