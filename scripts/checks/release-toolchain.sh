#!/usr/bin/env bash
# CI step: Check the toolchain is the one the release workflow names
# release.yml names it in each job that sets Go up (the package job, and the CI at the tag job whose
# runner the CI evidence is compared with): every one names the same version, and this job runs it.
set -e
named="$(sed -n "s/^ *go-version: *['\"]\{0,1\}\([0-9][0-9.]*\)['\"]\{0,1\} *\$/\1/p" .github/workflows/release.yml | sort -u)"
[ -n "$named" ] && [ "$(printf '%s\n' "$named" | wc -l)" -eq 1 ] || { echo "release.yml names Go $(printf '%s' "${named:-nothing}" | tr '\n' ' '); its jobs must name one version"; exit 1; }
[ "go$named" = "$(go env GOVERSION)" ] || { echo "release.yml names Go ${named}; this job runs $(go env GOVERSION)"; exit 1; }
