#!/usr/bin/env bash
# CI step: Check every program is named in the release build
# Names in a file compared with directories: an early warning only. The
# release workflow checks the archives themselves.
set -e
want="$(ls cmd | sort)"
have="$(sed -n 's/^    binary: //p' .goreleaser.yml | sort)"
[ "$want" = "$have" ] || { echo "cmd and .goreleaser.yml name different programs:"; diff <(echo "$want") <(echo "$have"); exit 1; }
