#!/usr/bin/env bash
# CI steps: check out and build the pinned Runtimes, the ownership-era Runner and the durable
# Gateway contract into .ci/ (ignored by git); the binaries land in .ci/bin. A checkout already at
# its pin is reused. Run from the repository root.
set -e
root=$(git rev-parse --show-toplevel)
bin=$root/.ci/bin
mkdir -p "$bin"

# fetch_pin REPOSITORY COMMIT DIR: DIR holds REPOSITORY at COMMIT.
fetch_pin() {
  [ "$(git -C "$3" rev-parse HEAD 2>/dev/null)" = "$2" ] && return 0
  rm -rf "$3"; mkdir -p "$3"
  git -C "$3" init -q
  git -C "$3" fetch -q --depth 1 "https://github.com/Judgment-Pack/$1.git" "$2"
  git -C "$3" checkout -q FETCH_HEAD
}

# Real-Runtime tests must run in CI, not silently skip behind an unset
# JPACK_TEST_BIN. Pin the independently versioned evaluator contract.
# A commit is pinned and not a tag, since a commit cannot come to name
# other code. The name after it is the release the commit was taken
# from when the pin was set: whoever moves one moves the other.
# v0.26.0 is the first Runtime release that chains and signs its trail,
# so TestTheRuntimesVerifierReadsTheChainOfRuns,
# TestASignedRunKeepsItsSignature and
# TestVerifyRunChecksARunTheRuntimeSigned run here and do not skip (#35).
# v0.30.0 is the first that refuses to sign with a seed whose public
# key is not JPACK_SIGNING_PUBLIC_KEY, which
# TestASeedReplacedAfterStartSignsNothing needs (#54). v0.31.0 is the
# first that takes a caller's binding (`--binding`, runtime ADR-0050),
# which the binding tests need (#55). A pin moved to a Runtime without
# `jpack audit` or `--binding` would skip them again, and the step after
# the tests fails if any of them does not pass.
# The tested Runtime: merged citation session identifier of any string (runtime #291).
fetch_pin judgment-pack-runtime 09206e743cdf410d7b5ee970f1c9d09ca7c015ca "$root/.ci/runtime"
# The build names the pin's release, as the release build does, so a
# test can tell what the Runtime under test answers by its release
# (TestTheRuntimesVerifierReadsTheChainOfRuns). Whoever moves the pin
# moves the version with it.
(cd "$root/.ci/runtime" && go build -trimpath -ldflags "-X github.com/Judgment-Pack/judgment-pack-runtime/internal/result.CLIVersion=0.33.0-dev.09206e7" -o "$bin/jpack" ./cmd/jpack)

# Dedicated retained-column companion: published v0.39.0 (the signed tag's
# commit). Its exact Spec v0.3.0/Base report (selector retained-column/1@27cb29bf)
# and both selected Core lifecycles are required separately from the historical
# ordinary and earlier Runtime contracts above/below.
fetch_pin judgment-pack-runtime 12793d2dd24d18549567ac422711a2b3c1489f04 "$root/.ci/runtime-retained"
(cd "$root/.ci/runtime-retained" && go build -trimpath -ldflags "-X github.com/Judgment-Pack/judgment-pack-runtime/internal/result.CLIVersion=0.39.0" -o "$bin/jpack-retained" ./cmd/jpack)

# Test-only old companion: its current-store refusal is part of the
# explicit manual-store preparation contract, not a release dependency.
fetch_pin judgment-pack-runner 1cc6b29885b344716a970cd21fefeaca0a4edaeb "$root/.ci/runner-old"
(cd "$root/.ci/runner-old" && go build -buildvcs=false -trimpath -o "$bin/jpack-runner-old" ./cmd/jpack-runner)

# A Runtime that signs and does not know the expected key, as every
# release from v0.26.0 to v0.29.x: Runner must name the key a replaced
# seed signed under (TestAnEarlierRuntimeSigningWithAReplacedSeedIsNamed,
# #54). It takes no binding either, as every release before v0.31.0:
# Runner must give it none, and verify-run report its record unbound
# (TestARuntimeWithoutTheBindingIsGivenNone,
# TestVerifyRunReportsARecordTheRuntimeDidNotBind, #55). Pinned by
# commit as the tested Runtime is. The commit is v0.27.1.
fetch_pin judgment-pack-runtime 7a2fcfe20cedba5f164246fc544c4920971d2c56 "$root/.ci/runtime-earlier"
(cd "$root/.ci/runtime-earlier" && go build -trimpath -ldflags "-X github.com/Judgment-Pack/judgment-pack-runtime/internal/result.CLIVersion=0.27.1" -o "$bin/jpack-earlier" ./cmd/jpack)

# The durable Gateway contract. The commit is v0.23.0 (the signed tag's
# commit), moved from v0.9.1 (#192). Its receipts may carry an
# acquisition's descriptorProvenance (gateway SPEC.md section 9), which
# the Runner's verifier accepts as a signed member it does not check
# against artifacts it does not hold.
fetch_pin judgment-pack-gateway 82d4f8ad9c5b38e5f27e9031c289d06434d1fab1 "$root/.ci/gateway"
(cd "$root/.ci/gateway/go" && go build -trimpath -o "$bin/gateway" .)
(cd "$root/.ci/gateway/adapters" && go build -trimpath -o "$bin/adapter-mcp" ./cmd/adapter-mcp)
