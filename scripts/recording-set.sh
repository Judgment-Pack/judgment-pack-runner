#!/usr/bin/env bash
# Record the Core 0.3.0 recording set (runner #174) into OUT, a directory that must not exist.
#
#   scripts/recording-set.sh OUT
#
# The set is the preparation vectors' scenarios recorded again under Core 0.3.0: the Runner's own
# tests run each scenario through the pinned Runtime that evaluates Core 0.3.0, with the triage pack
# declaring 0.3.0 (JPACK_RECORDING_CORE), and tools/preparation-vectors -core 0.3.0 makes the
# vectors of their exports as it made vectors/preparation, running verify-run, built from this
# commit, on each. Nothing in the set is edited by hand. vectors/preparation is not touched.
#
# The manifest records this commit, the Runtime's version and commit, the Spec release whose Core the
# packs declare, the signing identity (the published RFC 8032 test key the tests sign every record
# under), each recording's digest and what a regeneration cannot reproduce: each run's instants, the
# keys made for each run, identifiers and salts, and every digest and signature over them. A
# regeneration gives the same vectors, files, alterations and verify-run findings, not the same bytes.
#
# Run from a clean checkout, so that the commit named is the code that recorded the set. The pinned
# Runtimes are built first (scripts/checks/build-pinned.sh, which needs network access).
set -euo pipefail
out=${1:?usage: scripts/recording-set.sh OUT}
root=$(git rev-parse --show-toplevel)
cd "$root"
if [ -e "$out" ]; then echo "recording-set: $out exists: the set is written into a new directory" >&2; exit 1; fi
if [ -n "$(git status --porcelain)" ]; then echo "recording-set: the checkout has changes: the set names the commit it was recorded at" >&2; exit 1; fi
out=$(cd "$(dirname "$out")" && pwd)/$(basename "$out")
scripts/checks/build-pinned.sh
spec=internal/retainedcolumns/testdata/v0.3.0/release-identity.json
read -r spec_version spec_commit < <(python3 -c 'import json,sys; i=json.load(open(sys.argv[1])); assert i["specVersion"]=="0.3.0"; print(i["tag"], i["sourceCommit"])' "$spec")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
go build -trimpath -o "$work/jpack-runner" ./cmd/jpack-runner
go run ./tools/preparation-vectors -core 0.3.0 -record \
  -runtime .ci/bin/jpack-retained -runner "$work/jpack-runner" \
  -runner-commit "$(git rev-parse HEAD)" -runtime-commit "$(git -C .ci/runtime-retained rev-parse HEAD)" \
  -spec-version "$spec_version" -spec-commit "$spec_commit" \
  -out "$out"
echo "recording-set: wrote $out"
