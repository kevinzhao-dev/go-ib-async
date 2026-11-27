#!/usr/bin/env bash
# Runs all unit + E2E tests with the race detector and cross-package coverage,
# writes coverage.out / coverage.txt / coverage.html, and fails if total
# statement coverage is below COVERAGE_MIN (default 90).
#
# Excluded from the measurement: cmd/ (binaries that need a live Gateway) and
# internal/fakegw (test infrastructure).
set -euo pipefail

MIN="${COVERAGE_MIN:-90}"
OUT="${COVERAGE_OUT:-coverage.out}"

PKGS=$(go list ./... | grep -v -e '/cmd/' -e '/internal/fakegw' | paste -sd, -)
go test -race -count=1 -covermode=atomic -coverpkg="$PKGS" -coverprofile="$OUT" ./...

go tool cover -func="$OUT" > coverage.txt
go tool cover -html="$OUT" -o coverage.html
TOTAL=$(awk '/^total:/ { sub("%", "", $3); print $3 }' coverage.txt)

echo "Total coverage: ${TOTAL}% (minimum ${MIN}%)"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Coverage: ${TOTAL}% (minimum ${MIN}%)"
    echo
    echo '| Package | Coverage |'
    echo '|---|---|'
    go test -count=1 -cover ./... 2>/dev/null |
      awk '/coverage:/ { for (i = 1; i <= NF; i++) if ($i == "coverage:") print "| " $2 " | " $(i + 1) " |" }'
  } >> "$GITHUB_STEP_SUMMARY"
fi

if ! awk -v t="$TOTAL" -v m="$MIN" 'BEGIN { exit (t + 0 >= m + 0) ? 0 : 1 }'; then
  echo "::error::Total coverage ${TOTAL}% is below the ${MIN}% minimum"
  exit 1
fi
