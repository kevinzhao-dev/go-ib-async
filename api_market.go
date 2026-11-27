package ibgo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/state"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// Market data types for ReqMarketDataType.
const (
	MarketDataLive          = 1
	MarketDataFrozen        = 2
	MarketDataDelayed       = 3
	MarketDataDelayedFrozen = 4
)

// ReqMarketDataType selects the market data type for subsequent requests:
// MarketDataLive (default), MarketDataFrozen (last data at the close),
// MarketDataDelayed (free 15-20 minute delayed data when there is no live
// subscription) or MarketDataDelayedFrozen. With delayed types TWS still
// sends live data for contracts the account is subscribed to. The choice is
// re-applied after an automatic reconnect.
func (ib *IB) ReqMarketDataType(marketDataType int) error {
	if marketDataType < MarketDataLive || marketDataType > MarketDataDelayedFrozen {
		return fmt.Errorf("ibgo: invalid market data type %d", marketDataType)
	}
	if err := ib.client.ReqMarketDataType(marketDataType); err != nil {
		return err
	}
	ib.mdType.Store(int64(marketDataType))
	return nil
}

// ReqMktData subscribes to market data for a contract. With snapshot or
// regulatorySnapshot set it requests a one-off snapshot instead (prefer
// ReqTickers, which waits for the snapshot to complete).
//
// genericTickList is a comma separated list of generic tick ids, e.g.
// "233" (RT volume), "236" (shortable), "106" (option implied vol).
// Subscriptions to the same qualified contract share one Ticker; they are
// re-requested automatically after a reconnect by ConnectWithReconnect.
func (ib *IB) ReqMktData(con *contract.Contract, genericTickList string, snapshot, regulatorySnapshot bool) (*market.Ticker, error) {
	if con == nil {
		return nil, errors.New("ibgo: ReqMktData needs a contract")
	}
	ticker := ib.tickerForContract(con)
	if snapshot || regulatorySnapshot {
		reqID := ib.client.GetReqID()
		ib.registerTickerReq(reqID, con, ticker)
		// Tracked so tickSnapshotEnd / errors can complete it.
		ib.state.StartReq(reqID, con)
		if err := ib.client.ReqMktData(reqID, con, genericTickList, snapshot, regulatorySnapshot, nil); err != nil {
			ib.state.CancelReq(reqID)
			ib.releaseReq(reqID)
			return nil, err
		}
		return ticker, nil
	}
	_, err := ib.startSub(&subscription{
		kind:   subMktData,
		con:    con,
		ticker: ticker,
		send: func(reqID int64) error {
			return ib.client.ReqMktData(reqID, con, genericTickList, false, false, nil)
		},
		cancel: ib.client.CancelMktData,
	})
	if err != nil {
		return nil, err
	}
	return ticker, nil
}

// CancelMktData cancels a ReqMktData subscription. The subscription is found
// by contract pointer first, then by contract key (conId).
func (ib *IB) CancelMktData(con *contract.Contract) {
	ib.endSubByContract(subMktData, "", con)
}

// ReqTickers requests market data snapshots for the contracts and waits
// until all of them completed. It always returns one ticker per contract
// (in order). The error joins the per-contract failures (e.g. missing
// market data permissions); tickers of failed contracts are still returned
// with whatever data arrived. Contracts should be qualified.
func (ib *IB) ReqTickers(ctx context.Context, contracts ...*contract.Contract) ([]*market.Ticker, error) {
	tickers := make([]*market.Ticker, len(contracts))
	ids := make([]int64, len(contracts))
	chans := make([]<-chan state.Result, len(contracts))
	var errs []error
	for i, con := range contracts {
		t := ib.tickerForContract(con)
		tickers[i] = t
		reqID := ib.client.GetReqID()
		ids[i] = reqID
		ib.registerTickerReq(reqID, con, t)
		chans[i] = ib.state.StartReq(reqID, con)
		if err := ib.client.ReqMktData(reqID, con, "", true, false, nil); err != nil {
			ib.state.CancelReq(reqID)
			ib.releaseReq(reqID)
			chans[i] = nil
			errs = append(errs, fmt.Errorf("%s: %w", con, err))
		}
	}
	for i, ch := range chans {
		if ch == nil {
			continue
		}
		if _, err := ib.awaitReq(ctx, ids[i], ch); err != nil {
			ib.releaseReq(ids[i])
			if ctx.Err() != nil {
				for _, id := range ids[i+1:] {
					ib.state.CancelReq(id)
					ib.releaseReq(id)
				}
				return tickers, ctx.Err()
			}
			errs = append(errs, fmt.Errorf("%s: %w", contracts[i], err))
		}
	}
	return tickers, errors.Join(errs...)
}

// Tick-by-tick data types for ReqTickByTickData.
const (
	TickByTickLast     = "Last"
	TickByTickAllLast  = "AllLast"
	TickByTickBidAsk   = "BidAsk"
	TickByTickMidPoint = "MidPoint"
)

// ReqTickByTickData subscribes to tick-by-tick data. Ticks arrive in
// ticker.TickByTicks (per update batch) and also update Last/Bid/Ask.
func (ib *IB) ReqTickByTickData(con *contract.Contract, tickType string, numberOfTicks int, ignoreSize bool) (*market.Ticker, error) {
	switch tickType {
	case TickByTickLast, TickByTickAllLast, TickByTickBidAsk, TickByTickMidPoint:
	default:
		return nil, fmt.Errorf("ibgo: invalid tick-by-tick type %q", tickType)
	}
	ticker := ib.tickerForContract(con)
	_, err := ib.startSub(&subscription{
		kind: subTickByTick, con: con, ticker: ticker, tag: tickType,
		send: func(reqID int64) error {
			return ib.client.ReqTickByTickData(reqID, con, tickType, numberOfTicks, ignoreSize)
		},
		cancel: ib.client.CancelTickByTickData,
	})
	if err != nil {
		return nil, err
	}
	return ticker, nil
}

// CancelTickByTickData cancels a ReqTickByTickData subscription.
func (ib *IB) CancelTickByTickData(con *contract.Contract, tickType string) {
	ib.endSubByContract(subTickByTick, tickType, con)
}

// ReqMktDepth subscribes to market depth (order book). The book is kept in
// ticker.DomBids / ticker.DomAsks; ticker.DomTicks holds the changes of the
// current update batch.
func (ib *IB) ReqMktDepth(con *contract.Contract, numRows int, isSmartDepth bool) (*market.Ticker, error) {
	ticker := ib.tickerForContract(con)
	tag := "exchange"
	if isSmartDepth {
		tag = "smart"
	}
	_, err := ib.startSub(&subscription{
		kind: subMktDepth, con: con, ticker: ticker, tag: tag,
		send: func(reqID int64) error {
			ticker.Lock()
			ticker.DomBids, ticker.DomAsks = nil, nil
			ticker.Unlock()
			return ib.client.ReqMktDepth(reqID, con, numRows, isSmartDepth, nil)
		},
		cancel: func(reqID int64) error { return ib.client.CancelMktDepth(reqID, isSmartDepth) },
	})
	if err != nil {
		return nil, err
	}
	return ticker, nil
}

// CancelMktDepth cancels a ReqMktDepth subscription.
func (ib *IB) CancelMktDepth(con *contract.Contract, isSmartDepth bool) {
	tag := "exchange"
	if isSmartDepth {
		tag = "smart"
	}
	ib.endSubByContract(subMktDepth, tag, con)
}

// ReqRealTimeBars subscribes to 5-second real-time bars (barSize must be 5).
// whatToShow is TRADES, MIDPOINT, BID or ASK.
func (ib *IB) ReqRealTimeBars(con *contract.Contract, barSize int, whatToShow string, useRTH bool) (*market.RealTimeBarList, error) {
	rtbl := &market.RealTimeBarList{
		Contract:   con,
		BarSize:    barSize,
		WhatToShow: whatToShow,
		UseRTH:     useRTH,
	}
	_, err := ib.startSub(&subscription{
		kind: subRealTimeBars, con: con, target: rtbl,
		send: func(reqID int64) error {
			return ib.client.ReqRealTimeBars(reqID, con, barSize, whatToShow, useRTH, nil)
		},
		cancel:    ib.client.CancelRealTimeBars,
		onRestart: func(reqID int64) { rtbl.Lock(); rtbl.ReqID = reqID; rtbl.Unlock() },
	})
	if err != nil {
		return nil, err
	}
	return rtbl, nil
}

// CancelRealTimeBars unsubscribes from real-time bars.
func (ib *IB) CancelRealTimeBars(rtbl *market.RealTimeBarList) {
	ib.endSub(func(s *subscription) bool { return s.target == rtbl })
}

// ReqHistoricalDataStream requests historical bars ending now and keeps the
// list up to date (keepUpToDate): it returns once the initial bars arrived;
// afterwards the last bar is revised and new bars are appended as they
// form, each change firing bars.UpdateEvent (bars.NewBar tells which).
// Cancel with CancelHistoricalData. The stream is re-requested after an
// automatic reconnect (the bars are then replaced by a fresh download).
func (ib *IB) ReqHistoricalDataStream(ctx context.Context, con *contract.Contract, durationStr, barSizeSetting, whatToShow string, useRTH bool, formatDate int) (*market.BarDataList, error) {
	bdl := &market.BarDataList{
		Contract:       con,
		DurationStr:    durationStr,
		BarSizeSetting: barSizeSetting,
		WhatToShow:     whatToShow,
		UseRTH:         useRTH,
		FormatDate:     formatDate,
		KeepUpToDate:   true,
	}
	var ch <-chan state.Result
	var firstID int64
	reqID, err := ib.startSub(&subscription{
		kind: subHistoricalBars, con: con, target: bdl,
		send: func(reqID int64) error {
			return ib.client.ReqHistoricalData(reqID, con, "", durationStr, barSizeSetting, whatToShow, useRTH, formatDate, true, nil)
		},
		cancel: ib.client.CancelHistoricalData,
		onRestart: func(reqID int64) {
			if firstID == 0 {
				// First request: track it so the caller can wait for it.
				firstID = reqID
				ch = ib.state.StartReq(reqID, con)
			}
			bdl.Lock()
			bdl.ReqID = reqID
			bdl.Unlock()
		},
	})
	if err != nil {
		if firstID != 0 {
			ib.state.CancelReq(firstID)
		}
		return nil, err
	}
	if _, err := ib.awaitReq(ctx, reqID, ch); err != nil {
		ib.CancelHistoricalData(bdl)
		return nil, err
	}
	return bdl, nil
}

// CancelHistoricalData stops the updates of a ReqHistoricalDataStream list.
func (ib *IB) CancelHistoricalData(bars *market.BarDataList) {
	ib.endSub(func(s *subscription) bool { return s.target == bars })
}

// HistoricalTicks is the result of ReqHistoricalTicks; the slice matching
// whatToShow is filled (TRADES: Last, BID_ASK: BidAsk, MIDPOINT: MidPoint).
type HistoricalTicks struct {
	MidPoint []market.HistoricalTick
	BidAsk   []market.HistoricalTickBidAsk
	Last     []market.HistoricalTickLast
}

// ReqHistoricalTicks requests up to 1000 historical ticks. Give either
// startDateTime or endDateTime ("20261120 15:59:00 US/Eastern" or
// "20261120-20:59:00" in UTC); whatToShow is TRADES, BID_ASK or MIDPOINT.
func (ib *IB) ReqHistoricalTicks(ctx context.Context, con *contract.Contract, startDateTime, endDateTime string, numberOfTicks int, whatToShow string, useRTH, ignoreSize bool) (*HistoricalTicks, error) {
	if (startDateTime == "") == (endDateTime == "") {
		return nil, errors.New("ibgo: ReqHistoricalTicks needs exactly one of startDateTime and endDateTime")
	}
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, con)
	if err := ib.client.ReqHistoricalTicks(reqID, con, startDateTime, endDateTime, numberOfTicks, whatToShow, useRTH, ignoreSize, nil); err != nil {
		ib.state.CancelReq(reqID)
		return nil, err
	}
	v, err := ib.awaitReq(ctx, reqID, ch)
	if err != nil {
		return nil, err
	}
	res := &HistoricalTicks{}
	for _, item := range resultItems(v) {
		switch t := item.(type) {
		case market.HistoricalTick:
			res.MidPoint = append(res.MidPoint, t)
		case market.HistoricalTickBidAsk:
			res.BidAsk = append(res.BidAsk, t)
		case market.HistoricalTickLast:
			res.Last = append(res.Last, t)
		}
	}
	return res, nil
}

// ReqHeadTimeStamp returns the earliest date for which historical data of
// the given type is available.
func (ib *IB) ReqHeadTimeStamp(ctx context.Context, con *contract.Contract, whatToShow string, useRTH bool) (time.Time, error) {
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, con)
	if err := ib.client.ReqHeadTimestamp(reqID, con, whatToShow, useRTH, 1); err != nil {
		ib.state.CancelReq(reqID)
		return time.Time{}, err
	}
	v, err := ib.awaitReq(ctx, reqID, ch)
	_ = ib.client.CancelHeadTimestamp(reqID)
	if err != nil {
		return time.Time{}, err
	}
	s, _ := v.(string)
	t := parseIBDatetime(s)
	if t.IsZero() {
		return t, fmt.Errorf("ibgo: unparseable head timestamp %q", s)
	}
	return t, nil
}

// --- handlers ---

func (ib *IB) subscriberFor(reqID int64) interface{} {
	ib.state.Mu.RLock()
	defer ib.state.Mu.RUnlock()
	return ib.state.ReqID2Subscriber[reqID]
}

func (ib *IB) handleRealtimeBar(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	bar := market.RealTimeBar{
		Time:   time.Unix(r.ReadInt(), 0).UTC(),
		Open:   r.ReadFloat(),
		High:   r.ReadFloat(),
		Low:    r.ReadFloat(),
		Close:  r.ReadFloat(),
		Volume: r.ReadFloat(),
		WAP:    r.ReadFloat(),
		Count:  int(r.ReadInt()),
	}
	if rtbl, ok := ib.subscriberFor(reqID).(*market.RealTimeBarList); ok {
		rtbl.Lock()
		rtbl.Bars = append(rtbl.Bars, bar)
		rtbl.Unlock()
		rtbl.UpdateEvent.Emit(rtbl)
	}
}

func (ib *IB) handleHistoricalData(r *protocol.FieldReader) {
	reqID := r.ReadInt() // no version field
	_ = r.ReadString()   // startDateStr
	_ = r.ReadString()   // endDateStr
	numBars := int(r.ReadInt())

	bars := make([]market.BarData, 0, numBars)
	for range numBars {
		bars = append(bars, market.BarData{
			Date:     parseBarDate(r.ReadString()),
			Open:     r.ReadFloat(),
			High:     r.ReadFloat(),
			Low:      r.ReadFloat(),
			Close:    r.ReadFloat(),
			Volume:   r.ReadFloat(),
			Average:  r.ReadFloat(),
			BarCount: int(r.ReadInt()),
		})
	}

	// keepUpToDate list: initial download or a re-download after reconnect.
	if bdl, ok := ib.subscriberFor(reqID).(*market.BarDataList); ok {
		bdl.Lock()
		bdl.Bars = append([]market.BarData(nil), bars...)
		bdl.NewBar = true
		bdl.Unlock()
		if !ib.state.Requests.Has(reqID) {
			bdl.UpdateEvent.Emit(bdl)
		}
	}
	ib.state.EndReq(reqID, bars, nil)
}

// handleHistoricalDataUpdate revises the last bar of a keepUpToDate list or
// appends a new one (ib_async semantics).
func (ib *IB) handleHistoricalDataUpdate(r *protocol.FieldReader) {
	reqID := r.ReadInt() // no version field
	bar := market.BarData{
		BarCount: int(r.ReadInt()),
		Date:     parseBarDate(r.ReadString()),
		Open:     r.ReadFloat(),
		Close:    r.ReadFloat(),
		High:     r.ReadFloat(),
		Low:      r.ReadFloat(),
		Average:  r.ReadFloat(),
		Volume:   r.ReadFloat(),
	}
	bdl, ok := ib.subscriberFor(reqID).(*market.BarDataList)
	if !ok {
		return
	}
	bdl.Lock()
	n := len(bdl.Bars)
	switch {
	case n > 0 && bar.Date.Before(bdl.Bars[n-1].Date):
		bdl.Unlock()
		return // stale update
	case n == 0 || bar.Date.After(bdl.Bars[n-1].Date):
		bdl.Bars = append(bdl.Bars, bar)
		bdl.NewBar = true
	case bdl.Bars[n-1] == bar:
		bdl.Unlock()
		return // unchanged
	default:
		bdl.Bars[n-1] = bar
		bdl.NewBar = false
	}
	bdl.Unlock()
	bdl.UpdateEvent.Emit(bdl)
}

func (ib *IB) handleHeadTimestamp(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	ib.state.EndReq(reqID, r.ReadString(), nil)
}

// handleHistoricalTicks (MIDPOINT): reqId, n, n x (time, unused, price,
// size), done.
func (ib *IB) handleHistoricalTicks(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	n := int(r.ReadInt())
	for range n {
		t := market.HistoricalTick{Time: time.Unix(r.ReadInt(), 0).UTC()}
		r.Skip(1)
		t.Price = r.ReadFloat()
		t.Size = r.ReadFloat()
		ib.state.AppendResult(reqID, t)
	}
	if r.ReadBool() {
		ib.state.EndReq(reqID, nil, nil)
	}
}

// handleHistoricalTicksBidAsk: reqId, n, n x (time, mask, bid, ask,
// bidSize, askSize), done.
func (ib *IB) handleHistoricalTicksBidAsk(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	n := int(r.ReadInt())
	for range n {
		t := market.HistoricalTickBidAsk{Time: time.Unix(r.ReadInt(), 0).UTC()}
		mask := r.ReadInt()
		t.TickAttribBidAsk = market.TickAttribBidAsk{AskPastHigh: mask&1 != 0, BidPastLow: mask&2 != 0}
		t.PriceBid = r.ReadFloat()
		t.PriceAsk = r.ReadFloat()
		t.SizeBid = r.ReadFloat()
		t.SizeAsk = r.ReadFloat()
		ib.state.AppendResult(reqID, t)
	}
	if r.ReadBool() {
		ib.state.EndReq(reqID, nil, nil)
	}
}

// handleHistoricalTicksLast: reqId, n, n x (time, mask, price, size,
// exchange, specialConditions), done.
func (ib *IB) handleHistoricalTicksLast(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	n := int(r.ReadInt())
	for range n {
		t := market.HistoricalTickLast{Time: time.Unix(r.ReadInt(), 0).UTC()}
		mask := r.ReadInt()
		t.TickAttribLast = market.TickAttribLast{PastLimit: mask&1 != 0, Unreported: mask&2 != 0}
		t.Price = r.ReadFloat()
		t.Size = r.ReadFloat()
		t.Exchange = r.ReadString()
		t.SpecialConditions = r.ReadString()
		ib.state.AppendResult(reqID, t)
	}
	if r.ReadBool() {
		ib.state.EndReq(reqID, nil, nil)
	}
}
