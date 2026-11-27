# go-ib-async

Go client for the Interactive Brokers TWS/Gateway API. Port of the Python [ib_async](https://github.com/ib-api-reloaded/ib_async) library.

> **Status:** market data, account, contract and historical APIs are verified
> against a live IB Gateway (read-only, server version 178). Order placement
> is tested end to end against a fake gateway only; run the paper-account
> trading test before trading live. Not full parity with ib_async: see
> [STATUS.md](STATUS.md).

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
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Connect to IB Gateway (paper: 4002, live: 4001; TWS: 7497/7496)
    if err := ib.Connect(ctx, "127.0.0.1", 4002, 1); err != nil {
        log.Fatal(err)
    }
    defer ib.Disconnect()

    // Fill in conId etc. (errors.Is: ErrContractNotFound, ErrAmbiguousContract)
    aapl := contract.Stock("AAPL", "SMART", "USD")
    if err := ib.QualifyContracts(ctx, aapl); err != nil {
        log.Fatal(err)
    }

    bars, err := ib.ReqHistoricalData(ctx, aapl, "", "5 D", "1 day", "TRADES", true, 1)
    if err != nil {
        log.Fatal(err)
    }
    for _, b := range bars {
        fmt.Printf("%s  O=%.2f H=%.2f L=%.2f C=%.2f\n",
            b.Date.Format("2006-01-02"), b.Open, b.High, b.Low, b.Close)
    }

    // Free delayed data when the account has no live subscription.
    ib.ReqMarketDataType(ibgo.MarketDataDelayed)
    tickers, err := ib.ReqTickers(ctx, aapl) // waits for the snapshot
    if err != nil {
        log.Print(err) // e.g. missing permissions; tickers are still returned
    }
    t := tickers[0].Snapshot() // consistent copy, safe from any goroutine
    fmt.Printf("bid=%v ask=%v last=%v close=%v\n", t.Bid, t.Ask, t.Last, t.Close)
}
```

## Market data

```go
ticker, _ := ib.ReqMktData(con, "233", false, false)      // streaming quotes
ib.ReqTickByTickData(con, ibgo.TickByTickBidAsk, 0, false) // same Ticker
ib.ReqMktDepth(con, 10, false)                              // ticker.DomBids / DomAsks
ticker.UpdateEvent.Subscribe(func(t *market.Ticker) {
    // runs on the reader goroutine: fields can be read directly here
    fmt.Println(t.Bid, t.Ask, len(t.TickByTicks))
})
// From other goroutines use ticker.Snapshot() (or RLock/RUnlock).

rt, _ := ib.ReqRealTimeBars(con, 5, "TRADES", false)
bars, _ := ib.ReqHistoricalDataStream(ctx, con, "1 D", "1 min", "TRADES", false, 1)
bars.UpdateEvent.Subscribe(func(b *market.BarDataList) { /* b.NewBar */ })

ib.ReqAccountUpdates(ctx, "")   // AccountValues(), Portfolio(), PortfolioEvent
ib.ReqPnL("", "")               // PnL(), PnLEvent
ib.ReqPnLSingle("", "", conID)  // PnLSingles(), PnLSingleEvent
```

Options: `ticker.ModelGreeks` / `BidGreeks` / `AskGreeks` / `LastGreeks`
(`nil` fields mean "not computed"). Other lookups: `ReqSecDefOptParams`,
`ReqMatchingSymbols`, `ReqMarketRule`, `ReqHeadTimeStamp`,
`ReqHistoricalTicks`, `ReqCurrentTime`.

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
    // called after every (re)connect
})
```

An automatic reconnect keeps orders, fills and positions, so `*order.Trade`
objects held by the application stay valid, and re-requests every streaming
subscription (market data, tick-by-tick, depth, bars, PnL, account updates)
and the market data type, so `Ticker` and bar list objects keep updating. `SyncOrders` then refreshes open
orders, fetches executions (missed fills are added to their trades),
finalises orders that completed while disconnected and refreshes positions.
After a process restart use `SyncOrders(ctx, SyncOptions{IncludeCompleted: true})`
and `journal.Reconcile(journal.Replay(records), ib.Trades(), clientID)` to
compare the journal with what the broker reports.

## What Works

| Feature | Status |
|---------|--------|
| Connect / Disconnect / handshake, reconnect + heartbeat | Verified live (incl. dropped connection and subscription restore) |
| Contract details, `QualifyContracts`, symbol search, market rules, option chains | Verified live |
| Historical bars (incl. keep-up-to-date), historical ticks, head timestamp | Verified live |
| Market data: snapshots (`ReqTickers`), streaming, delayed data, greeks, tick-by-tick, depth, real-time bars | Verified live |
| Positions, account summary, account updates / portfolio, PnL, PnL single | Verified live |
| Read-only order queries (`ReqAllOpenOrders`, `ReqExecutions`, `SyncOrders`) | Verified live (read-only API mode) |
| Read-only API mode detection (`ErrReadOnlyAPI`) | Verified live |
| Place / Modify / Cancel order, partial fills, commissions | E2E-tested against fake gateway |
| Bracket orders, OCA helper, WhatIf, completed orders | E2E-tested against fake gateway |
| Pre-trade risk manager (`risk`) | Unit + E2E tested |
| Persistent order journal (`journal`) | Unit + E2E tested |

"E2E-tested against fake gateway" means the real client (protocol, decoders,
state, events) runs against `internal/fakegw`, an in-process TWS fake that
speaks the v178 wire protocol (layouts follow ib_async). A live paper-account
trading test exists (`integration_trading_test.go`) but must be run by you:

```bash
IB_E2E_TRADING=1 go test -tags=integration -v -run TestIntegrationTrading .
```

See [STATUS.md](STATUS.md) for full details.

### Testing against a live account safely

`cmd/ibguard` is a proxy that blocks every order message (place, cancel,
global cancel, exercise, ...) at the socket level, and `cmd/smoketest` runs
~45 read-only checks with sanity checks on the decoded values:

```bash
go run ./cmd/ibguard -upstream 127.0.0.1:4001 -listen 127.0.0.1:4011 &
go run ./cmd/smoketest -port 4011 -guard-pid $(pgrep -x ibguard)
```

`-guard-pid` enables the reconnect check (the guard drops the connection on
SIGUSR1). Pass the guard's own pid, which it logs at startup; under `go run`,
`$!` is the `go` command, which does not forward the signal.

`cmd/mdcheck` shows which market data the account receives through the API.
It requests live data (no delayed fallback) for US stocks on each network,
OPRA options, CME/CBOT/NYMEX/COMEX/CFE futures and the SPX index, plus
tick-by-tick, 5-second bars and market depth, and prints a verdict per line
(`OK live quotes`, `OK no quotes (closed?)`, `NOT SUBSCRIBED`, `DELAYED`, ...):

```bash
go run ./cmd/mdcheck -port 4011
```

Run it during US market hours to see quotes: outside them stocks and options
have no live quotes and stock tick-by-tick / 5-second bars stay empty, without
any permission error.

Keep "Read-Only API" enabled in TWS/Gateway while developing against a live
account: orders, `ReqOpenOrders` and `ReqCompletedOrders` are then rejected
(the errors match `ErrReadOnlyAPI`; `SyncOrders` falls back to
`ReqAllOpenOrders`).

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
  protocol/          TCP connection, wire encoder/decoder, client
  internal/state/    State manager, request tracking, tick maps
  internal/fakegw/   Fake TWS for end-to-end tests
```

**Concurrency model:** Single reader goroutine processes all inbound messages
and runs event callbacks (keep callbacks short; hand work to your own
goroutines). Request-response matching via per-request buffered channels.
State maps are guarded by a `sync.RWMutex`; `Trade`, `Ticker`, `BarDataList`
and `RealTimeBarList` have their own locks (`Snapshot()` for reads from other
goroutines). Order placement is serialised so risk checks cannot be raced.

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
make smoke            # read-only live smoke test (needs Gateway; see above)
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

- **Order paths not yet verified against a live (paper) account.** Run the paper trading test above before trading live.
- **Partial message coverage.** Scanners, news, fundamental data, histograms, FA and WSH requests are not implemented yet; see STATUS.md.
- Errors not tied to a request (e.g. data farm status) only reach `ErrorEvent`; streaming subscriptions that fail (e.g. no permissions) stay registered until cancelled.

## Requirements

- Go 1.24+
- IB TWS or Gateway (for live testing)

## License

BSD 2-Clause. See [LICENSE](LICENSE).
