#!/usr/bin/env bash
# Runs every fuzz target in ./protocol for FUZZTIME each (default 10s).
#
# The Go fuzzing engine sometimes fails a run that found nothing with only
# "context deadline exceeded" when -fuzztime runs out (seen in CI on main and
# on PRs, on different targets). A real finding always writes the failing
# input under testdata/fuzz and says "Failing input written to", so a run
# that failed that way and wrote nothing is retried once; any other failure,
# or a second one, fails the job.
set -euo pipefail

FUZZTIME="${FUZZTIME:-10s}"
log=$(mktemp)
trap 'rm -f "$log"' EXIT

fuzz() {
  go test -run='^$' -fuzz="^$1\$" -fuzztime="$FUZZTIME" ./protocol/ 2>&1 | tee "$log"
}

for target in $(go test -list '^Fuzz' ./protocol/ | grep '^Fuzz'); do
  echo "=== $target ($FUZZTIME)"
  if fuzz "$target"; then
    continue
  fi
  if grep -q 'context deadline exceeded' "$log" && ! grep -q 'Failing input written to' "$log"; then
    echo "::warning::$target: the fuzzing engine stopped with 'context deadline exceeded' and no failing input; retrying once"
    fuzz "$target"
    continue
  fi
  exit 1
done
