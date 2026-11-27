# Guide

How to use the library: connecting, market data, orders, and staying in sync
across reconnects. For the AI agent server see [MCP.md](MCP.md); for testing
against a live account see [LIVE_TESTING.md](LIVE_TESTING.md).

- [Connecting](#connecting)
- [Contracts](#contracts)
- [Market data](#market-data)
- [Account and portfolio](#account-and-portfolio)
- [Orders](#orders)
- [Reconnect and order synchronisation](#reconnect-and-order-synchronisation)
- [Concurrency model](#concurrency-model)
- [Known limitations](#known-limitations)

## Connecting

```go
ib := ibgo.New()
if err := ib.Connect(ctx, "127.0.0.1", 4002, 1); err != nil {
    log.Fatal(err)
}
defer ib.Disconnect()
```

Ports: IB Gateway 4002 (paper) / 4001 (live), TWS 7497 (paper) / 7496 (live).
Enable the API in TWS/Gateway (Configure → API → Settings). Every blocking call
takes a `context.Context`; for a connection that heals itself use
[`ConnectWithReconnect`](#reconnect-and-order-synchronisation).

## Contracts

```go
aapl := contract.Stock("AAPL", "SMART", "USD")
if err := ib.QualifyContracts(ctx, aapl); err != nil {
    // errors.Is(err, ibgo.ErrContractNotFound / ibgo.ErrAmbiguousContract)
}
```

`QualifyContracts` fills in `ConID` and the other fields in place. Other
constructors: `contract.Option`, `contract.Future`, `contract.Forex`, ...
Lookups: `ReqContractDetails`, `ReqMatchingSymbols`, `ReqSecDefOptParams`
(option chains), `ReqMarketRule`.

## Market data

```go
// Snapshot: waits until the data has arrived.
ib.ReqMarketDataType(ibgo.MarketDataDelayed) // free delayed data without a subscription
tickers, err := ib.ReqTickers(ctx, aapl)
if err != nil {
    log.Print(err) // e.g. missing permissions; tickers are still returned
}
t := tickers[0].Snapshot() // consistent copy, safe from any goroutine
fmt.Println(t.Bid, t.Ask, t.Last, t.Close)

// Streaming: one live Ticker per contract, updated in place.
ticker, _ := ib.ReqMktData(con, "233", false, false)
ib.ReqTickByTickData(con, ibgo.TickByTickBidAsk, 0, false) // same Ticker
ib.ReqMktDepth(con, 10, false)                              // ticker.DomBids / DomAsks (one depth stream per contract)
ticker.UpdateEvent.Subscribe(func(t *market.Ticker) {
    // runs on the reader goroutine: fields can be read directly here
    fmt.Println(t.Bid, t.Ask, len(t.TickByTicks))
})
// From other goroutines use ticker.Snapshot() (or RLock/RUnlock).
```

Options: `ticker.ModelGreeks` / `BidGreeks` / `AskGreeks` / `LastGreeks`
(`nil` fields mean "not computed").

### Bars and ticks

```go
bars, err := ib.ReqHistoricalData(ctx, aapl, "", "5 D", "1 day", "TRADES", true, 1)

stream, _ := ib.ReqHistoricalDataStream(ctx, con, "1 D", "1 min", "TRADES", false, 1)
stream.UpdateEvent.Subscribe(func(b *market.BarDataList) { /* b.NewBar */ })

rt, _ := ib.ReqRealTimeBars(con, 5, "TRADES", false)
```

Also: `ReqHistoricalTicks`, `ReqHeadTimeStamp`, `ReqCurrentTime`.

## Account and portfolio

```go
ib.ReqAccountUpdates(ctx, "")   // AccountValues(), Portfolio(), PortfolioEvent
ib.ReqPnL("", "")               // PnL(), PnLEvent
ib.ReqPnLSingle("", "", conID)  // PnLSingles(), PnLSingleEvent
```

`ReqPositions` and `ReqAccountSummary` are one-off requests.

## Orders

```go
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

Before anything is sent, the risk manager must accept the order (a rejection
is a `*risk.Violation`, see `risk.IsViolation`) and the journal must have
recorded it (fsync'd). Order placement is serialised, so concurrent orders
cannot race past the limits. `rm.Halt` is a kill switch; `MaxDailyLoss`
trips it automatically.

Read trade state through the locked accessors (`trade.Status()`,
`trade.StatusSnapshot()`, `trade.FillsSnapshot()`, `trade.FilledQty()`),
not the struct fields: trades are updated on the reader goroutine.

## Reconnect and order synchronisation

```go
cfg := ibgo.DefaultReconnectConfig() // heartbeat + SyncOrders after each connect
go ib.ConnectWithReconnect(ctx, "127.0.0.1", 4002, 1, cfg, func() {
    // called after every (re)connect
})
```

An automatic reconnect keeps orders, fills and positions, so `*order.Trade`
objects held by the application stay valid, and re-requests every streaming
subscription (market data, tick-by-tick, depth, bars, PnL, account updates)
and the market data type, so `Ticker` and bar list objects keep updating.
Do not re-request them in `onConnect`: that would create a second
subscription. Subscriptions made on the new connection (e.g. from a
`ConnectedEvent` handler) are not sent twice, get the market data type
chosen before the reconnect and share the contract's existing `Ticker`; one
whose re-send fails is retried after the next reconnect.

`IsConnected` is true only once TWS has accepted the client id and sent the
next order id and the managed accounts. `LastConnectError` tells why the
last attempt failed; `errors.Is(err, ibgo.ErrClientIDInUse)` detects TWS
error 326, another API client connected with the same client id. A client
that places no orders can set `cfg.ClientIDFallbacks` to try the following
ids instead (`ClientID` reports the one in use); one that places orders
should not, because TWS binds orders to the client id that placed them.

When the account updates download of the new connection completes, positions
closed while disconnected leave `Portfolio()`. `SyncOrders` then refreshes
open orders, fetches executions (missed fills are added to their trades),
finalises orders that completed while disconnected and refreshes positions.

After a process restart use `SyncOrders(ctx, SyncOptions{IncludeCompleted: true})`
and `journal.Reconcile(journal.Replay(records), ib.Trades(), clientID)` to
compare the journal with what the broker reports.

## Concurrency model

- A single reader goroutine processes all inbound messages and runs event
  callbacks. Keep callbacks short and hand work to your own goroutines.
- Requests are matched to responses through per-request buffered channels.
- State maps are guarded by a `sync.RWMutex`; `Trade`, `Ticker`,
  `BarDataList` and `RealTimeBarList` have their own locks. Use
  `Snapshot()` (or the `*Snapshot()` accessors) to read them from other
  goroutines.
- Outbound messages are throttled to 45 per second.

## Known limitations

- Order paths are tested end to end against a fake gateway, not yet against a
  live (paper) account. Run the [paper trading test](LIVE_TESTING.md#paper-trading-test)
  before trading live.
- Scanners, news, fundamental data, histograms, FA and WSH requests are not
  implemented yet; see [STATUS.md](../STATUS.md).
- Errors not tied to a request (e.g. data farm status) only reach
  `ErrorEvent`; streaming subscriptions that fail (e.g. no permissions) stay
  registered until cancelled.
