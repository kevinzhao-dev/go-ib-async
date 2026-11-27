# go-ib-async

Go client for the Interactive Brokers TWS/Gateway API. Port of the Python [ib_async](https://github.com/ib-api-reloaded/ib_async) library.

> **Status: Core subset. Not production-ready. Not full parity with ib_async.**
> See [STATUS.md](STATUS.md) for the complete parity matrix including what's implemented, partial, and missing.

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    ibgo "github.com/kevinzhao-dev/go-ib-async"
    "github.com/kevinzhao-dev/go-ib-async/contract"
)

func main() {
    ib := ibgo.New()
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    // Connect to IB Gateway (paper: 4002, live: 4001)
    if err := ib.Connect(ctx, "127.0.0.1", 4002, 1); err != nil {
        log.Fatal(err)
    }
    defer ib.Disconnect()

    // Request contract details
    details, err := ib.ReqContractDetails(ctx, contract.Stock("AAPL", "SMART", "USD"))
    if err != nil {
        log.Fatal(err)
    }
    if len(details) == 0 {
        log.Fatal("no contract details returned")
    }
    fmt.Printf("AAPL conId=%d\n", details[0].Contract.ConID)

    // Request historical data
    bars, err := ib.ReqHistoricalData(ctx, details[0].Contract,
        "", "5 D", "1 day", "TRADES", true, 1)
    if err != nil {
        log.Fatal(err)
    }
    for _, b := range bars {
        fmt.Printf("%s  O=%.2f H=%.2f L=%.2f C=%.2f\n",
            b.Date.Format("2006-01-02"), b.Open, b.High, b.Low, b.Close)
    }

    // Market data snapshot (idiomatic: use event, not sleep)
    ticker, err := ib.ReqMktData(details[0].Contract, "", true, false)
    if err != nil {
        log.Fatal(err)
    }
    ch := ticker.UpdateEvent.Once()
    <-ch // wait for first tick update
    fmt.Printf("bid=%.2f ask=%.2f last=%.2f\n", ticker.Bid, ticker.Ask, ticker.Last)
}
```

## Trading

```go
con := details[0].Contract

// Optional safety layers: pre-trade risk checks and a write-ahead journal.
rm := risk.New(risk.Limits{
    MaxOrderQuantity:    500,
    MaxPositionQuantity: 1000,
    MaxOrdersPerWindow:  10, // per second
    MaxDailyLoss:        2000,
}, ib)
ib.SetRiskManager(rm)

j, err := journal.Open("orders-2026-11-26.jsonl", journal.Options{})
if err != nil {
    log.Fatal(err)
}
defer j.Close()
ib.SetJournal(j)

// What-if: margin/commission impact without placing.
st, err := ib.WhatIfOrder(ctx, con, order.LimitOrder("BUY", 100, 150))

// Place, modify, cancel.
trade, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 100, 150))
filled := trade.FilledEvent.Once()
trade.FillEvent.Subscribe(func(f *order.Fill) { /* partial fill */ })

mod := *trade.Order
mod.LmtPrice = 150.5
ib.PlaceOrder(con, &mod) // same orderId = modification

ib.CancelOrder(trade.Order)

// Bracket: limit entry + take-profit + stop-loss, released together.
b, err := ib.BracketOrder("BUY", 100, 150, 160, 145)
legs, err := ib.PlaceBracketOrder(con, b)
```

Read trade state through the locked accessors (`trade.Status()`,
`trade.StatusSnapshot()`, `trade.FillsSnapshot()`, `trade.FilledQty()`),
not the struct fields: trades are updated on the reader goroutine.

### Reconnect and order synchronisation

```go
cfg := ibgo.DefaultReconnectConfig() // heartbeat + SyncOrders after each connect
go ib.ConnectWithReconnect(ctx, "127.0.0.1", 4002, 1, cfg, func() {
    // re-subscribe market data here
})
```

An automatic reconnect keeps orders, fills and positions, so `*order.Trade`
objects held by the application stay valid. `SyncOrders` then refreshes open
orders, fetches executions (missed fills are added to their trades),
finalises orders that completed while disconnected and refreshes positions.
After a process restart use `SyncOrders(ctx, SyncOptions{IncludeCompleted: true})`
and `journal.Reconcile(journal.Replay(records), ib.Trades(), clientID)` to
compare the journal with what the broker reports.

## AI agents (MCP)

`cmd/ib-mcp` exposes the client to AI agents as a
[Model Context Protocol](https://modelcontextprotocol.io) server over stdio:
contracts, quotes, bars, option chains, positions, account values, orders and
executions, plus a what-if `preview_order`. It is read-only by default.
Trading (`-enable-trading`) is restricted to paper accounts. It requires a
journal and risk limits, and works in two steps: `preview_order` returns a
single-use `preview_id`, and `place_order` sends exactly that order through
the risk manager. Agents can halt trading but cannot resume it.

```bash
go install github.com/kevinzhao-dev/go-ib-async/cmd/ib-mcp@latest
claude mcp add ib -- ib-mcp -port 4002
```

See [docs/MCP.md](docs/MCP.md) for the tools, the safety model and the
configuration.

## What Works

| Feature | Status |
|---------|--------|
| Connect / Disconnect / handshake | Verified live |
| Contract Details, Historical Data, Market Data (snapshot) | Verified live |
| Positions, Account Summary, Option Chains | Verified live (positions decoder fixed since) |
| Place / Modify / Cancel order, partial fills, commissions | E2E-tested against fake gateway |
| Open order sync (`ReqOpenOrders`, `ReqAllOpenOrders`, `ReqExecutions`, `ReqCompletedOrders`, `SyncOrders`) | E2E-tested against fake gateway |
| Bracket orders, OCA helper, WhatIf | E2E-tested against fake gateway |
| Reconnect with heartbeat + post-connect sync | E2E-tested against fake gateway |
| Pre-trade risk manager (`risk`) | Unit + E2E tested |
| Persistent order journal (`journal`) | Unit + E2E tested |
| Real-Time Bars | Implemented, not verified |
| MCP server for AI agents (`mcpserver`, `cmd/ib-mcp`) | E2E-tested against fake gateway |

"E2E-tested against fake gateway" means the real client (protocol, decoders,
state, events) runs against `internal/fakegw`, an in-process TWS fake that
speaks the v178 wire protocol (layouts follow ib_async). A live paper-account
trading test exists (`integration_trading_test.go`) but must be run by you:

```bash
IB_E2E_TRADING=1 go test -tags=integration -v -run TestIntegrationTrading .
```

See [STATUS.md](STATUS.md) for full details.

## Architecture

```
ibgo (root)          IB facade - Connect, public API, message dispatch
  contract/          Contract, Stock, Option, Future, etc.
  order/             Order, Trade, OrderStatus, conditions, brackets
  market/            Ticker, BarData, OptionComputation, depth
  account/           AccountValue, Position, PnL, news
  event/             Generic Event[T] pub/sub
  risk/              Pre-trade risk checks, kill switch, daily loss limit
  journal/           Append-only JSONL order journal, replay, reconcile
  mcpserver/         MCP server (stdio) exposing the client to AI agents
  protocol/          TCP connection, wire encoder/decoder, client
  internal/state/    State manager, request tracking, tick maps
  internal/fakegw/   Fake TWS for end-to-end tests
```

**Concurrency model:** Single reader goroutine processes all inbound messages
and runs event callbacks. Request-response matching via per-request buffered
channels. State maps are guarded by a `sync.RWMutex`; `Trade` has its own lock
and accessor methods. Order placement is serialised so risk checks cannot be
raced.

## Development

```bash
make                  # build + vet + test (no Gateway needed)
make ci               # everything CI runs: build, lint, shuffled race tests, coverage gate
make lint             # go vet (incl. integration tag), gofmt, go mod tidy -diff
make test-race        # tests with race detector
make test-shuffle     # race detector + random test order
make cover-check      # coverage.out/.txt/.html, fails below COVERAGE_MIN (default 90%)
make fuzz-all         # every fuzz target, FUZZTIME each (default 10s)
make test-live        # integration tests (needs Gateway running)
make smoke            # live smoke test (needs Gateway)
make bench            # benchmarks
```

### Tests

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

### CI

GitHub Actions (`.github/workflows/ci.yml`) runs on pushes and PRs to `main`:

- **test (1.24 / 1.25)**: build, `go vet` (also with `-tags=integration`), `go test -race -shuffle=on ./...`. Required for merging into `main`.
- **lint**: gofmt and `go mod tidy -diff`.
- **coverage**: `scripts/coverage.sh` with a 90% minimum; uploads `coverage.html` as an artifact and posts a per-package table to the job summary.
- **fuzz**: every fuzz target for 10s.

## Known Limitations

- **Not production-ready for live trading.** The trading paths are tested against a fake gateway, not yet against a live account.
- **Ticker and bar-list fields are not locked.** They are written on the reader goroutine; read them inside `UpdateEvent`/`PendingTickersEvent` callbacks, or accept racy reads.
- **Market data subscriptions are not restored** after a reconnect; re-subscribe in the `onConnect` callback.
- **Partial message coverage.** Many advanced request types (tick-by-tick, scanners, news, PnL subscriptions, ...) are not implemented yet; see STATUS.md.

## Requirements

- Go 1.24+
- IB TWS or Gateway (for live testing)

## License

BSD 2-Clause. See [LICENSE](LICENSE).
