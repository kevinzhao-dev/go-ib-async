# go-ib-async Parity Status

**Status: market data, account, contract and historical APIs verified against a
live IB Gateway (server version 178, read-only API mode, November 2026). Order
placement is E2E-tested against a fake gateway only. Not full parity with
ib_async.**

Legend: **live** = verified against a real IB Gateway; **e2e** = exercised end to
end against `internal/fakegw` (the real client vs. a fake TWS speaking the v178
wire protocol); **unit** = unit tests only.

## Feature Status

| Feature | Status | Notes |
|---------|--------|-------|
| TCP connection / handshake | live, e2e | Dial honours context; `ErrAlreadyConnected` on double connect; state resets when the peer closes |
| Reconnection / heartbeat | live, e2e | Auto-reconnect keeps orders/fills/positions, re-requests every streaming subscription and the market data type; optional post-connect `SyncOrders` |
| Contract details, `QualifyContracts` | live | Not found / ambiguous errors (`ErrContractNotFound`, `ErrAmbiguousContract`, `*QualifyError`) |
| `ReqMatchingSymbols`, `ReqMarketRule`, `ReqSecDefOptParams` | live | |
| Historical bars | live | Intraday dates with time zone; `formatDate=2` in UTC |
| `ReqHistoricalDataStream` (keepUpToDate) | live, e2e | Last bar revised / new bar appended (`NewBar`), `CancelHistoricalData` |
| `ReqHistoricalTicks` (TRADES / BID_ASK / MIDPOINT), `ReqHeadTimeStamp` | live | |
| Market data: `ReqMktData`, `ReqTickers` (snapshots), `ReqMarketDataType` | live | Real-time (US stocks, OPRA, CME/CBOT/NYMEX/COMEX futures) and delayed ticks, tickReqParams (minTick, BBO exchange), one Ticker per contract; permissions per venue: `cmd/mdcheck` |
| Option greeks (tickOptionComputation) | live | Bid/ask/last/model greeks; TWS sentinels map to nil |
| `ReqTickByTickData`, `ReqMktDepth` | live | |
| `ReqRealTimeBars` | live | |
| Positions, account summary | live, e2e | |
| `ReqAccountUpdates`, `Portfolio`, `PortfolioEvent` | live | |
| `ReqPnL`, `ReqPnLSingle` | live | |
| `ReqCurrentTime` | live | |
| Read-only API mode | live | `ErrReadOnlyAPI`; `SyncOrders` falls back to reqAllOpenOrders |
| `ReqAllOpenOrders`, `ReqExecutions`, `SyncOrders` | live (read-only), e2e | |
| `ReqOpenOrders`, `ReqCompletedOrders` | e2e | Rejected by TWS in read-only mode (fail fast with `ErrReadOnlyAPI`) |
| PlaceOrder / ModifyOrder / CancelOrder | e2e | Risk check → journal write-ahead → register → send, serialised |
| Order rejects / server cancels | e2e, unit | Error codes on an order id cancel the trade; 321 on a new order rejects it |
| Partial fill tracking, completed orders | e2e, unit | |
| Bracket order helper, WhatIf | e2e | |
| Persistent order journal | unit, e2e | `journal` package |
| Risk management | unit, e2e | `risk` package |
| Request throttling | unit | 45 msg/s sliding window |
| End-to-end trading test | e2e; live test provided | `integration_trading_test.go` (paper account, opt-in, **not yet run**) |

## Public API

### Not Implemented (present in Python ib_async)
| Method | Priority |
|--------|----------|
| ReqScannerSubscription / ReqScannerParameters | medium |
| ReqNewsProviders / ReqNewsArticle / ReqHistoricalNews / news ticks | low |
| ReqHistogramData | low |
| ReqFundamentalData | low |
| ReqPositionsMulti / ReqAccountUpdatesMulti | low |
| ReqFamilyCodes / ReqSmartComponents / ReqMktDepthExchanges / ReqSoftDollarTiers | low |
| FA (RequestFA / ReplaceFA), WSH, user info | low |
| FlexReport, IBC/Watchdog | low |

## Inbound Message Handler Coverage

Handled: tickPrice(1), tickSize(2), orderStatus(3), errMsg(4), openOrder(5),
updateAccountValue(6), updatePortfolio(7), updateAccountTime(8), nextValidId(9),
contractDetails(10), execDetails(11), updateMktDepth(12), updateMktDepthL2(13),
managedAccounts(15), historicalData(17), tickOptionComputation(21),
tickGeneric(45), tickString(46), tickEFP(47), currentTime(49), realtimeBar(50),
contractDetailsEnd(52), openOrderEnd(53), accountDownloadEnd(54),
execDetailsEnd(55), tickSnapshotEnd(57), marketDataType(58),
commissionReport(59), position(61), positionEnd(62), accountSummary(63),
accountSummaryEnd(64), secDefOptParams(75), secDefOptParamsEnd(76),
symbolSamples(79), tickReqParams(81), headTimestamp(88),
historicalDataUpdate(90), marketRule(93), pnl(94), pnlSingle(95),
historicalTicks(96), historicalTicksBidAsk(97), historicalTicksLast(98),
tickByTick(99), orderBound(100), completedOrder(101), completedOrdersEnd(102).

Field layouts were checked against ib_async's `decoder.py` (server version
157–178) and, for every message marked live above, against captured Gateway
traffic.

## Fixed after live Gateway testing (November 2026)

1. **Inbound message ids 79–85 were misnumbered** (symbolSamples, tickReqParams, ...): tickReqParams (81) was decoded as symbolSamples and completed unrelated pending requests, e.g. market data snapshots ended before their data arrived.
2. Error 321 ("Error validating request") was treated as a warning, so an invalid request (bad bar size, end date with ADJUSTED_LAST, ...) hung until its context expired. It now fails the request; on a new order it rejects the order.
3. In read-only API mode TWS rejects reqOpenOrders/reqCompletedOrders with reqId -1 and no end message: `ReqOpenOrders`, `ReqCompletedOrders` and `SyncOrders` hung forever. They now fail fast with `ErrReadOnlyAPI`, and `SyncOrders` falls back to reqAllOpenOrders.
4. Request errors were `*RequestError` while `ErrorEvent` delivered `*IBError`; `RequestError` is now an alias of `IBError` (carries the contract and the advanced reject JSON).
5. Option greeks (tickOptionComputation), tickReqParams, tickEFP, tick-by-tick, depth, PnL and head timestamp messages were silently dropped.
6. 10090/10091 ("part of the requested market data needs a subscription") ended snapshots although the rest of the data keeps coming; they are warnings now.
7. `historicalDataUpdate` appended a bar for every update instead of revising the last bar.
8. Snapshot requests were never released after tickSnapshotEnd.
9. Ticker / bar-list fields were written without a lock (data race with application goroutines).

## Fixed in the earlier November 2026 review

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

- `market.Ticker` embeds `market.TickerData` (field access unchanged; struct literals must use `TickerData{...}`); `Snapshot()` returns a `TickerData` copy. `HasBidAsk`/`Midpoint`/`MarketPrice` are value methods of `TickerData`.
- `RequestError` is a type alias of `IBError`; its message reads `ibgo: error <code> ...`.
- Subscriptions to the same qualified contract share one `Ticker`.
- Unix-epoch times (`formatDate=2` bars, real-time bars, tick timestamps) are in UTC instead of the local zone.
- `account.PnLSingle.Position` is a `float64`.
- `IB.CancelOrder(ord)` returns `(*order.Trade, error)`.
- `protocol.Client.ServerVersion` is a method (`SetServerVersion` for tests).
- Unsolicited `openOrder` messages fire `OpenOrderEvent` (was `OrderStatusEvent`).
- `ConnectWithReconnect` keeps trades across reconnects (explicit `Disconnect` still clears all state); `DefaultReconnectConfig` enables `SyncOrders`.

## Known Gaps

1. The trading paths have not been run against a live (paper) account yet.
2. A streaming subscription rejected by TWS (e.g. missing permissions) stays registered (and is re-sent after a reconnect) until cancelled.
3. With real-time subscriptions, option quotes and stock tick-by-tick / 5-second bars have only been checked outside US market hours (permissions confirmed, no data yet); futures were verified streaming.
4. See "Not Implemented" above.

## Test Coverage

Total library coverage (all packages except `cmd/` and `internal/fakegw`,
measured across packages by `scripts/coverage.sh`): **94.2%**. CI fails below 90%.

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
| ibgo (root) | 91.7% |
| internal/fakegw (test infrastructure) | 73.2% own tests; exercised further by the E2E tests |
| cmd/ibguard | blocking of every order message type is tested |

Found while adding tests (fixed): `reqHeadTimestamp` sent `whatToShow`/`useRTH`
swapped, `reqScannerSubscription` sent its two option lists swapped, and the
BAG contract key ignored leg actions (a spread and its reverse collided).
