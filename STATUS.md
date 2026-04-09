# go-ib-async Parity Status

**Status: Core subset implemented. NOT full parity with ib_async.**

## API Method Parity

### Implemented & Live-Verified
| Method | Status | Notes |
|--------|--------|-------|
| Connect/Disconnect | ✅ verified | Handshake, startApi, nextValidId, managedAccounts |
| ReqPositions | ✅ verified | Position tracking with account/conId maps |
| ReqAccountSummary | ✅ verified | Tag-based account queries |
| ReqContractDetails | ✅ verified | Full 40+ field parser |
| ReqHistoricalData | ✅ verified | Daily/intraday bars, date parsing |
| ReqMktData (snapshot) | ✅ verified | Bid/ask/last ticks working |
| ReqSecDefOptParams | ✅ verified | Option chain queries |
| CancelMktData | ✅ implemented | |
| PlaceOrder | ✅ implemented | Full wire encoding including all fields |
| CancelOrder | ✅ implemented | |

### Implemented but NOT Live-Verified
| Method | Status | Notes |
|--------|--------|-------|
| ReqRealTimeBars | ⚠️ partial | Handler + subscription, not tested live. No public CancelRealTimeBars verified. |
| ReqAccountUpdates | ⚠️ protocol only | Client.ReqAccountUpdates sends, handler exists, but NO public IB.ReqAccountUpdates method |
| ReqOpenOrders | ⚠️ protocol only | Client.ReqOpenOrders sends, openOrder decoder done, but NO public IB.ReqOpenOrders method |

### NOT Implemented (Missing from Python ib_async)
| Method | Priority | Python Location |
|--------|----------|-----------------|
| QualifyContracts | high | ib.py:2110 |
| ReqHistoricalTicks | medium | ib.py:1354 |
| ReqTickByTickData | medium | ib.py:1290 |
| ReqHeadTimeStamp | medium | ib.py:1322 |
| ReqHistogramData | medium | ib.py:1392 |
| ReqFundamentalData | low | ib.py:1410 |
| ReqScannerSubscription | low | ib.py:1434 |
| ReqNewsProviders | low | ib.py:1502 |
| ReqNewsArticle | low | ib.py:1510 |
| ReqMatchingSymbols | low | ib.py:1530 |
| ReqMarketRule | low | ib.py:1540 |
| WhatIfOrder | medium | ib.py:870 |
| BracketOrder | medium | ib.py:900 |
| ReqPnL/ReqPnLSingle | medium | ib.py:1550 |
| ReqCompletedOrders | low | ib.py:1590 |
| FlexReport | low | flexreport.py |
| IBC/Watchdog | low | ibcontroller.py |

## Inbound Message Handler Coverage

**Handled: 29 / 82 message types defined**

Covered (in handleMessage dispatch): tickPrice(1), tickSize(2), orderStatus(3), errMsg(4), openOrder(5), updateAccountValue(6), updatePortfolio(7), nextValidId(9), contractDetails(10), execDetails(11), managedAccounts(15), historicalData(17), tickGeneric(45), tickString(46), realtimeBar(50), contractDetailsEnd(52), openOrderEnd(53), accountDownloadEnd(54), execDetailsEnd(55), tickSnapshotEnd(57), marketDataType(58), commissionReport(59), position(61), positionEnd(62), accountSummary(63), accountSummaryEnd(64), secDefOptParams(75), secDefOptParamsEnd(76), historicalDataUpdate(90)

NOT in dispatch (despite being claimed previously): updateAccountTime(8), currentTime(49)

Not covered: bondContractDetails, scannerData, tickOptionComputation, tickEFP, fundamentalData, deltaNeutralValidation, tickReqParams, symbolSamples, mktDepthExchanges, tickNews, newsProviders, newsArticle, historicalNews, headTimestamp, histogramData, pnl, pnlSingle, historicalTicks, tickByTick, orderBound, completedOrder, completedOrdersEnd, wshMetaData, wshEventData, historicalSchedule, userInfo, and others

## Known Gaps (from GPT review)

1. **tickString**: does not parse timestamp, RT volume, fundamental ratios, dividends
2. **Events**: UpdateEvent and PendingTickersEvent declared but never emitted
3. **Throttle**: config constants defined, no runtime enforcement
4. **BarDataList lifecycle**: ReqHistoricalData returns []BarData, not BarDataList with keepUpToDate
5. **State locking**: Mu exported for cross-package access; works but not ideal long-term

## Test Coverage

| Package | Coverage |
|---------|----------|
| account | 100% |
| event | 100% |
| market | 100% |
| contract | 97.7% |
| order | 90.9% |
| protocol | 65.1% |
| state | 64.5% |
| ibgo (root) | 6.1% (handlers need live TWS) |
