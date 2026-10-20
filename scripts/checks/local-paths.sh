#!/usr/bin/env bash
# No machine-local path in tracked text: a /home/<name> or /tmp/<name> path of someone's machine.
# Not counted: the files below, where a /tmp path is a synthetic example, test input or a command
# written in a design note before this check existed. A new file is checked.
set -e
if git grep -nE '(^|[^A-Za-z0-9_])/(home|tmp)/[A-Za-z0-9_.-]+' -- . ':!*.png' ':!*.jpg' \
  ':!docs/design/executor-ownership.md' \
  ':!docs/design/manual-admission-store-prepare-shape.md' \
  ':!docs/design/manual-admission-store-prepare-wire.json' \
  ':!internal/runner/cloud_test.go'; then
  echo 'a machine-local path is in tracked text'; exit 1
fi
