#!/usr/bin/env bash
# CI step: the postgresql store kind's tests (docs/design/store-postgresql.md,
# section 10, and TestStoreKindsConform on the kind), against the server
# JPACK_TEST_POSTGRESQL names: the connection string of an administrator that
# is not a superuser and may create roles and databases. The `PostgreSQL store`
# job of ci.yml names one, and sets JPACK_TEST_POSTGRESQL_REQUIRED, by which a
# test that would skip fails. Without a server named, and not required, the
# step says so and runs nothing.
set -e
if [ -z "${JPACK_TEST_POSTGRESQL:-}" ]; then
  if [ -n "${JPACK_TEST_POSTGRESQL_REQUIRED:-}" ]; then
    echo "postgresql-store: JPACK_TEST_POSTGRESQL_REQUIRED is set and no server is named in JPACK_TEST_POSTGRESQL"
    exit 1
  fi
  echo "postgresql-store: no server is named in JPACK_TEST_POSTGRESQL; nothing ran"
  exit 0
fi
export JPACK_TEST_POSTGRESQL_REQUIRED=1
out="$(go test -race -count=1 -v -timeout 20m -run '^(TestStoreKindsConform|TestPostgreSQL)' ./internal/runner)" || { printf '%s\n' "$out"; exit 1; }
printf '%s\n' "$out" | grep -E '^(ok|FAIL|---) ' || true
# The other kinds' cases skip what a kind does not hold; the postgresql kind's
# and the TestPostgreSQL tests skip only without a server.
if printf '%s\n' "$out" | grep -E -q -- '--- SKIP: (TestStoreKindsConform/postgresql|TestPostgreSQL)'; then
  echo "postgresql-store: a test of the postgresql kind was skipped"
  exit 1
fi
for kind in postgresql postgresql-twice; do
  printf '%s\n' "$out" | grep -q -- "^[[:space:]]*--- PASS: TestStoreKindsConform/$kind " || { echo "postgresql-store: TestStoreKindsConform/$kind was not among the passes"; exit 1; }
done
tests=$(grep -ho '^func TestPostgreSQL[A-Za-z0-9_]*' internal/runner/*_test.go | sed 's/^func //' | sort -u)
[ -n "$tests" ] || { echo "postgresql-store: no TestPostgreSQL test found"; exit 1; }
for name in $tests; do
  printf '%s\n' "$out" | grep -q -- "^--- PASS: $name " || { echo "postgresql-store: $name was not among the passes"; exit 1; }
done
echo "postgresql-store: the postgresql kind's conformance and its $(printf '%s\n' "$tests" | wc -l) TestPostgreSQL tests passed"
