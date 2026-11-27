# go-ib-async Parity Status

**Status: Core subset implemented. NOT full parity with ib_async.**

Legend: **live** = verified against a real IB Gateway; **e2e** = exercised end to
end against `internal/fakegw` (the real client vs. a fake TWS speaking the v178
wire protocol); **unit** = unit tests only.

## Trading Feature Status

| Feature | Status | Notes |
|---------|--------|-------|
| TCP connection / handshake | live, e2e | Dial honours context; `ErrAlreadyConnected` on double connect; state resets when the peer closes |
| Contract details | live | |
| Historical data | live | Intraday dates with time zone now parsed (`parseIBDatetime`) |
| Market data | live (snapshot) | Unqualified contracts (conId 0) no longer collide |
| Account / positions | live, e2e | **Fixed:** position decoder skipped `localSymbol`/`tradingClass` |
| Option chains | live | |
| PlaceOrder | e2e | Risk check → journal write-ahead → register → send, serialised |
| ModifyOrder | e2e | `PlaceOrder` with an existing orderId; "Modified" ack logged; done orders rejected (`ErrOrderDone`) |
| CancelOrder | e2e | Returns the trade; PendingCancel/Cancelled + events |
| Order rejects / server cancels | e2e, unit | Error codes on an order id cancel the trade (ib_async semantics); warnings keep it live |
| Partial fill tracking | e2e, unit | **Fixed:** execDetails decoder skipped a non-existent version field |
| Completed orders | e2e | |
| Request throttling | unit | 45 msg/s sliding window |
| Reconnection / heartbeat | e2e | Auto-reconnect keeps orders/fills/positions; optional post-connect `SyncOrders` |
| Open order synchronization | e2e | `ReqOpenOrders`, `ReqAllOpenOrders`, `ReqExecutions`, `ReqAutoOpenOrders`, `SyncOrders` |
| Bracket order helper | e2e | `BracketOrder` (validated prices) + `PlaceBracketOrder` (pre-checks all legs, rolls back on failure); `order.OneCancelsAll` |
| WhatIf order | e2e | `WhatIfOrder` returns `*order.OrderState`; never creates a trade |
| Persistent order journal | unit, e2e | `journal` package: fsync'd JSONL, write-ahead submit, crash-tolerant load, `Replay`, `Reconcile` |
| Risk management | unit, e2e | `risk` package: qty, notional, worst-case position, open orders, rate, duplicates, price collar, symbol/account lists, kill switch, daily loss |
| Market data snapshot (blocking) | e2e | `ReqTickerSnapshot` waits for tickSnapshotEnd (signalled after the batch's ticks are applied) and detaches the ticker; `ReqMarketDataType` |
| MCP server for AI agents | e2e | `mcpserver` + `cmd/ib-mcp`: stdio MCP, read tools, preview → place with single-use previews, paper-only by default; see docs/MCP.md |
| End-to-end trading test | e2e; live test provided | `e2e_test.go` (CI); `integration_trading_test.go` (paper account, opt-in, **not yet run**) |

## Public API

### Implemented
Connect, Disconnect, ConnectWithReconnect, ReqContractDetails, ReqHistoricalData,
ReqMktData, CancelMktData, ReqTickerSnapshot, ReqMarketDataType, ReqRealTimeBars, CancelRealTimeBars, ReqPositions,
ReqAccountSummary, ReqSecDefOptParams, PlaceOrder, CancelOrder, ReqGlobalCancel,
WhatIfOrder, BracketOrder, PlaceBracketOrder, ReqOpenOrders, ReqAllOpenOrders,
ReqAutoOpenOrders, ReqExecutions, ReqCompletedOrders, SyncOrders,
SetRiskManager, SetJournal.

### Not Implemented (present in Python ib_async)
| Method | Priority | Python Location |
|--------|----------|-----------------|
| QualifyContracts | high | ib.py |
| ReqAccountUpdates (public) | medium | ib.py |
| ReqPnL / ReqPnLSingle | medium | ib.py |
| ReqHistoricalTicks | medium | ib.py |
| ReqTickByTickData | medium | ib.py |
| ReqHeadTimeStamp | medium | ib.py |
| ReqHistogramData | medium | ib.py |
| ReqFundamentalData | low | ib.py |
| ReqScannerSubscription | low | ib.py |
| ReqNewsProviders / ReqNewsArticle | low | ib.py |
| ReqMatchingSymbols | low | ib.py |
| ReqMarketRule | low | ib.py |
| FlexReport | low | flexreport.py |
| IBC/Watchdog | low | ibcontroller.py |

## Inbound Message Handler Coverage

Handled: tickPrice(1), tickSize(2), orderStatus(3), errMsg(4), openOrder(5),
updateAccountValue(6), updatePortfolio(7), nextValidId(9), contractDetails(10),
execDetails(11), managedAccounts(15), historicalData(17), tickGeneric(45),
tickString(46), currentTime(49), realtimeBar(50), contractDetailsEnd(52),
openOrderEnd(53), accountDownloadEnd(54), execDetailsEnd(55), tickSnapshotEnd(57),
marketDataType(58), commissionReport(59), position(61), positionEnd(62),
accountSummary(63), accountSummaryEnd(64), secDefOptParams(75),
secDefOptParamsEnd(76), historicalDataUpdate(90), orderBound(100),
completedOrder(101), completedOrdersEnd(102).

Field layouts of every handled message were re-checked against ib_async's
`decoder.py` (server version 157–178).

## Fixed in the November 2026 review

Critical (wrong data on a live connection):
1. `execDetails` skipped a version field that servers ≥136 don't send: every fill was shifted by one field.
2. `position` skipped `localSymbol`/`tradingClass`: position quantity was read from the wrong field (and usually removed as 0).
3. Order errors (e.g. 201 rejected) never updated the trade: rejected orders stayed `PendingSubmit` forever.
4. Concurrent map writes between request starters and the reader goroutine (`Results`, `PendingTickers`) could crash the process.
5. `event.Once` registered its callback before its id was known (data race, leaked subscriptions).

Other:
- Warning codes (399, 404, 10167, ...) failed requests; now handled like ib_async.
- Duplicate `orderStatus`/`execDetails` messages re-fired events.
- `IsConnected()` stayed true after the peer closed the socket; `Done()` raced with reconnect.
- Concurrent `ReqPositions`/`ReqCompletedOrders` could complete each other's requests.
- Heartbeat read `LastTime` without synchronisation; heartbeat timeouts wiped all state.
- `ReqAccountSummary` left the TWS subscription open (max 2 allowed).
- Execution/bar timestamps with a time zone suffix or two spaces parsed as zero time.
- `CancelMktData` sent while holding the state lock; unqualified contracts collided.
- `-tags='!integration'` in the Makefile and CI was a no-op.

## Breaking API changes

- `IB.CancelOrder(ord)` now returns `(*order.Trade, error)`.
- `protocol.Client.ServerVersion` is a method (`SetServerVersion` for tests).
- Unsolicited `openOrder` messages fire `OpenOrderEvent` (was `OrderStatusEvent`).
- `ConnectWithReconnect` keeps trades across reconnects (explicit `Disconnect` still clears all state); `DefaultReconnectConfig` enables `SyncOrders`.

## Known Gaps

1. Ticker / bar-list fields are written on the reader goroutine without a lock.
2. Market data subscriptions are not restored after reconnect.
3. BarDataList with keepUpToDate is not exposed publicly.
4. The trading paths have not been run against a live (paper) account yet.

## Test Coverage

Total library coverage (all packages except `cmd/` and `internal/fakegw`,
measured across packages by `scripts/coverage.sh`): **96.3%**. CI fails below 90%.

| Package | Coverage |
|---------|----------|
| account | 100% |
| contract | 100% |
| event | 100% |
| internal/state | 100% |
| internal/throttle | 100% |
| market | 100% |
| order | 100% |
| risk | 98.1% |
| protocol | 96.5% |
| journal | 95.2% |
| mcpserver | 96.8% |
| ibgo (root) | 93.6% |
| internal/fakegw (test infrastructure) | 73.2% own tests; exercised further by the E2E tests |

Found while adding tests (fixed): `reqHeadTimestamp` sent `whatToShow`/`useRTH`
swapped, `reqScannerSubscription` sent its two option lists swapped, and the
BAG contract key ignored leg actions (a spread and its reverse collided).
