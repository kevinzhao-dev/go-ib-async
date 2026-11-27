#!/usr/bin/env bash
# Runs every fuzz target in ./protocol for FUZZTIME each (default 10s).
set -euo pipefail

FUZZTIME="${FUZZTIME:-10s}"
for target in $(go test -list '^Fuzz' ./protocol/ | grep '^Fuzz'); do
  echo "=== $target ($FUZZTIME)"
  go test -run='^$' -fuzz="^${target}\$" -fuzztime="$FUZZTIME" ./protocol/
done
