# Development

No gateway is needed for the default build and tests: the end-to-end tests
run the real client against `internal/fakegw`, a fake TWS.

## Make targets

```bash
make                  # build + vet + test (no Gateway needed)
make ci               # everything CI runs: build, lint, shuffled race tests, coverage gate
make lint             # go vet (incl. integration tag), gofmt, go mod tidy -diff
make test-race        # tests with race detector
make test-shuffle     # race detector + random test order
make cover-check      # coverage.out/.txt/.html, fails below COVERAGE_MIN (default 90%)
make fuzz-all         # every fuzz target, FUZZTIME each (default 10s)
make test-live        # integration tests (needs Gateway running)
make smoke            # read-only live smoke test (needs Gateway; see LIVE_TESTING.md)
make bench            # benchmarks
```

## Tests

| Kind | Where | What it checks |
|------|-------|----------------|
| Wire format (outbound) | `protocol/requests_test.go`, `order_wire_test.go` | Every request and `placeOrder` field-by-field against ib_async's `client.py` layout, incl. server-version branches |
| Wire format (inbound) | `handlers_test.go`, `order_wire_test.go` | Every handled message decoded from the ib_async `decoder.py` layout (ticks, contract details, bars, account, portfolio, openOrder with all optional blocks, conditions) |
| Client / connection | `protocol/client_test.go`, `review_fixes_test.go` | Handshake, version checks, disconnect/reconnect, framing limits, panic recovery |
| Order lifecycle | `order/`, `internal/state/`, `review_fixes_test.go` | Trade status semantics, duplicates, rejects, warnings, concurrency (`-race`) |
| Risk / journal | `risk/`, `journal/` | Every limit, kill switch, daily loss; write-ahead, crash recovery, replay, reconcile |
| End to end | `e2e_test.go`, `api_misc_test.go` | Real client against `internal/fakegw` (fake TWS): fills, modify/cancel, brackets, whatIf, reconnect sync, restart recovery |
| Fuzz | `protocol/fuzz_test.go` | Decoder/encoder robustness |
| Live (opt-in) | `integration*_test.go` (`//go:build integration`) | Real Gateway; `IB_E2E_TRADING=1` places paper orders |

Per-package coverage is listed in [STATUS.md](../STATUS.md#test-coverage).

## CI

GitHub Actions (`.github/workflows/ci.yml`) runs on pushes and PRs to `main`:

- **test (1.24 / 1.25)**: build, `go vet` (also with `-tags=integration`), `go test -race -shuffle=on ./...`. Required for merging into `main`.
- **lint**: gofmt and `go mod tidy -diff`.
- **coverage**: `scripts/coverage.sh` with a 90% minimum; uploads `coverage.html` as an artifact and posts a per-package table to the job summary.
- **fuzz**: every fuzz target for 10s.
