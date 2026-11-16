#!/usr/bin/env bash
# CI step: Unit, conformance and cross-process recovery tests
set -e
root=$(git rev-parse --show-toplevel)
bin=$root/.ci/bin
# Keep cumulative per-package work below Go's default timeout in each invocation.
# The retained artifact-boundary work and ordinary workflow work have independent
# budgets; the other ordinary tests are divided by name. These selections are
# disjoint and exhaustive, including future names, examples and fuzz seed corpus.
# Every invocation keeps race instrumentation and the default package timeout.
retained='^(TestColumn|TestSelectedLeaf)'
# CI runs the groups in shards, each in a job of its own (ci.yml, `Unit tests
# (<os>, i/n)`): JPACK_UNIT_SHARD=i/n runs only the groups of shard i, and only
# shard i runs the store conformance run (`store`) when its row names it. Rows
# name groups by their number in unit_groups below. Every group and `store` are
# in exactly one row, and every run, sharded or not, refuses a table where that
# does not hold before it starts anything. Without JPACK_UNIT_SHARD every group
# and the store conformance run run, as before the shards. The rows are balanced
# by the groups' times on main, three at once on Linux (and two at once on
# macOS): 758 s (507), 708 s (448), 714 s (479), 687 s (539) (#269).
unit_shards=('1 3' '2 5 9 store' '4 8 10' '6 7 11')
shard=
if [ -n "${JPACK_UNIT_SHARD:-}" ]; then
  if [[ $JPACK_UNIT_SHARD =~ ^([1-9][0-9]*)/([1-9][0-9]*)$ ]] &&
    [ "${BASH_REMATCH[2]}" = "${#unit_shards[@]}" ] && [ "${BASH_REMATCH[1]}" -le "${#unit_shards[@]}" ]; then
    shard=${BASH_REMATCH[1]}
  else
    echo "unit-tests: JPACK_UNIT_SHARD is '$JPACK_UNIT_SHARD', not i/${#unit_shards[@]} with i from 1 to ${#unit_shards[@]}"
    exit 2
  fi
fi
# The groups do not depend on one another and run side by side, at most
# JPACK_UNIT_TEST_JOBS at once: 3 by default, 2 on macOS, whose hosted runner
# has fewer cores, and 2 in a shard on either; 1 runs them one after another.
# Two runs of one package at once share nothing on disk: the tests write under
# t.TempDir and listen on ports the system picks. Each group's output goes to a file of its own and is
# printed whole only when the group fails; once a group has failed no further
# group starts, the running ones finish, and the step fails. Each group prints
# one line with its time when it ends, and the last line of the groups (the one
# check.sh's summary shows) lists every group, numbered in the order below, with
# its result and time.
case "$(uname -s)" in Darwin) jobs=2 ;; *) jobs=3 ;; esac
[ -z "$shard" ] || jobs=2
jobs=${JPACK_UNIT_TEST_JOBS:-$jobs}
case "$jobs" in
  '' | *[!0-9]* | 0*) echo "unit-tests: JPACK_UNIT_TEST_JOBS is '$jobs', not a whole number above 0"; exit 2 ;;
esac
# Whether this run runs group (or `store`) $1: every one without a shard.
in_shard() {
  [ -z "$shard" ] && return 0
  case " ${unit_shards[shard - 1]} " in *" $1 "*) return 0 ;; esac
  return 1
}
groups=0 counting=1 running=0 failed=0
group_pid=() start=() result=() took=() args=() ran=()
run_unit() {
  groups=$((groups + 1))
  if [ "$counting" = 1 ] || ! in_shard "$groups"; then return 0; fi
  finish
  while [ "$running" -ge "$jobs" ]; do
    read -r -t 5 -u 3 ended || true
    finish
  done
  args[groups]="$*"
  ran[${#ran[@]}]=$groups
  if [ "$failed" != 0 ]; then result[groups]='not run'; return 0; fi
  start[groups]=$SECONDS
  (
    status=0
    unit_go "$@" >"$logs/$groups.log" 2>&1 3>&- || status=$?
    echo "$status" >"$logs/$groups.st" && mv "$logs/$groups.st" "$logs/$groups.status"
    echo "$groups" >&3
  ) &
  group_pid[groups]=$!
  running=$((running + 1))
}
# Record every running group that has ended.
finish() {
  local i status
  for i in "${!group_pid[@]}"; do
    if [ ! -f "$logs/$i.status" ] && kill -0 "${group_pid[i]}" 2>/dev/null; then continue; fi
    wait "${group_pid[i]}" 2>/dev/null || true
    status=$(cat "$logs/$i.status" 2>/dev/null) || status='ended without a status'
    unset 'group_pid[i]'
    running=$((running - 1))
    took[i]=$((SECONDS - start[i]))
    if [ "$status" = 0 ]; then
      result[i]=ok
    else
      result[i]="FAIL ($status)"
      failed=$((failed + 1))
    fi
    echo "group $i ${result[i]} in ${took[i]}s: ${args[i]}"
    if [ "$status" != 0 ]; then
      echo "== the output of group $i =="
      cat "$logs/$i.log"
      echo "== the end of the output of group $i =="
    fi
  done
}
unit_go() {
  JPACK_TEST_BIN="$bin/jpack" \
  JPACK_RETAINED_TEST_BIN="$bin/jpack-retained" \
  JPACK_COVERS_TEST_BIN="$bin/jpack-covers" \
  JPACK_CORE050_TEST_BIN="$bin/jpack-core050" \
  JPACK_EARLIER_TEST_BIN="$bin/jpack-earlier" \
  JPACK_GATEWAY_TEST_BIN="$bin/gateway" \
  JPACK_MCP_ADAPTER_TEST_BIN="$bin/adapter-mcp" \
  JPACK_OLD_RUNNER_TEST_BIN="$bin/jpack-runner-old" \
  go test -race "$@" ./...
}
# The offline release/run cases each traverse four 40 MiB artifacts. Give them
# separate budgets; the exact complement retains record cases and future children.
# Large export/wire fixtures also run apart from the remaining ColumnRole work.
offline='^TestColumnRoleOfflineIndependentArtifactLimits$'
large_roles='^TestColumnRole(OfflineIndependentArtifactLimits|StoredAndExportBoundaries|LiteralSeparatorWireCustody)$'
unit_groups() {
  run_unit -run "$offline/^release$"
  run_unit -run "$offline/^run$"
  run_unit -run "$offline" -skip "$offline/^(release|run)$"
  run_unit -run '^TestColumnRoleStoredAndExportBoundaries$'
  run_unit -run '^TestColumnRoleLiteralSeparatorWireCustody$'
  run_unit -run '^TestColumnRole' -skip "$large_roles"
  run_unit -run "$retained" -skip '^TestColumnRole'
  run_unit -run '^TestWorkflow'
  # Tests named A... are more than half of the tests from A to M. As one group
  # beside two others they took 604 s of the test binary's 600 s in one run, so
  # they are a group of their own (#269).
  run_unit -run '^TestA'
  run_unit -run '^Test[B-M]' -skip "$retained"
  run_unit -skip '^(TestColumn|TestSelectedLeaf|TestWorkflow|Test[A-M])'
}
# The shard table names every group and `store` once, and nothing else.
unit_groups
total=$groups
seen=' '
for row in "${unit_shards[@]}"; do
  [ -n "$row" ] || { echo "unit-tests: a shard of the table has no group"; exit 2; }
  for item in $row; do
    case "$item" in
      store) ;;
      '' | *[!0-9]* | 0*) echo "unit-tests: the shard table names '$item', which is not a group"; exit 2 ;;
      *) [ "$item" -le "$total" ] || { echo "unit-tests: the shard table names group $item of $total"; exit 2; } ;;
    esac
    case "$seen" in *" $item "*) echo "unit-tests: the shard table names $item twice"; exit 2 ;; esac
    seen="$seen$item "
  done
done
for item in $(seq 1 "$total") store; do
  case "$seen" in *" $item "*) ;; *) echo "unit-tests: the shard table names $item in no shard"; exit 2 ;; esac
done
logs=$(mktemp -d "${TMPDIR:-/tmp}/unit-tests.XXXXXX")
trap 'wait; rm -rf "$logs"' EXIT
# A group that ends writes its number here, which wakes the wait for a free
# place; the status files say which groups ended, and how.
mkfifo "$logs/ended"
exec 3<>"$logs/ended"
groups=0 counting=0 began=$SECONDS
unit_groups
while [ "$running" != 0 ]; do
  read -r -t 5 -u 3 ended || true
  finish
done
line=
for i in "${ran[@]}"; do
  line="$line${line:+, }$i ${result[i]}${took[i]:+ ${took[i]}s}"
done
if [ -z "$shard" ]; then
  echo "unit-tests: $groups groups, at most $jobs at once, in $((SECONDS - began))s: $line"
else
  echo "unit-tests: shard $shard/${#unit_shards[@]}: ${#ran[@]} of $groups groups, at most $jobs at once, in $((SECONDS - began))s: $line"
fi
exec 3>&-
[ "$failed" = 0 ] || exit 1
if ! in_shard store; then
  for i in "${!unit_shards[@]}"; do
    case " ${unit_shards[i]} " in *" store "*) echo "unit-tests: store conformance runs in shard $((i + 1))/${#unit_shards[@]}" ;; esac
  done
  exit 0
fi
# The store contract's conformance suite runs against every kind
# (docs/design/store-interface.md), and against the in-memory kind with every
# transaction run twice (docs/design/store-postgresql.md, D2): a kind whose
# cases do not run and pass fails this step, as a skipped test would
# otherwise pass.
began=$SECONDS
out="$(go test -race -count=1 -v -run '^TestStoreKindsConform$' ./internal/runner)" || { printf '%s\n' "$out"; exit 1; }
echo "unit-tests: store conformance in $((SECONDS - began))s"
for kind in sqlite memory twice; do
  printf '%s\n' "$out" | grep -q -- "^[[:space:]]*--- PASS: TestStoreKindsConform/$kind " || { echo "TestStoreKindsConform/$kind did not run and pass"; exit 1; }
done
echo "store conformance: the sqlite, memory and twice kinds ran and passed"
