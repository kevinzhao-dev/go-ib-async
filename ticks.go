package ibgo

// Market data tick handlers. Every write to a Ticker happens on the reader
// goroutine while holding the ticker's lock (see market.Ticker).

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/internal/state"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// tickerFor returns the ticker of a market data request id, or nil.
func (ib *IB) tickerFor(reqID int64) *market.Ticker {
	ib.state.Mu.RLock()
	defer ib.state.Mu.RUnlock()
	return ib.state.ReqID2Ticker[reqID]
}

// updateTicker applies f to the ticker of reqID under its lock and marks it
// as updated in the current batch.
func (ib *IB) updateTicker(reqID int64, f func(t *market.Ticker)) {
	t := ib.tickerFor(reqID)
	if t == nil {
		return
	}
	t.Lock()
	f(t)
	t.Unlock()
	ib.state.MarkTickerPending(t)
}

// unsetToNaN maps TWS's "no value" encodings (UNSET_DOUBLE, empty) to NaN.
func unsetToNaN(v float64) float64 {
	if v == UnsetDouble || math.IsInf(v, 0) {
		return math.NaN()
	}
	return v
}

func (ib *IB) handleTickPrice(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	price := r.ReadFloat()
	size := unsetToNaN(r.ReadFloat())
	_ = r.ReadString() // attrib mask (canAutoExecute, pastLimit, preOpen)

	ib.updateTicker(reqID, func(t *market.Ticker) {
		switch tickType {
		case 1, 66: // bid / delayed bid
			t.PrevBid, t.PrevBidSize = t.Bid, t.BidSize
			t.Bid, t.BidSize = price, size
		case 2, 67: // ask / delayed ask
			t.PrevAsk, t.PrevAskSize = t.Ask, t.AskSize
			t.Ask, t.AskSize = price, size
		case 4, 68: // last / delayed last
			t.PrevLast, t.PrevLastSize = t.Last, t.LastSize
			t.Last, t.LastSize = price, size
		default:
			if field, ok := state.PriceTickMap[tickType]; ok {
				setTickerFloat(t, field, price)
			}
		}
		t.Ticks = append(t.Ticks, market.TickData{Time: ib.now(), TickType: tickType, Price: price, Size: size})
	})
}

func (ib *IB) handleTickSize(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	size := unsetToNaN(r.ReadFloat())

	ib.updateTicker(reqID, func(t *market.Ticker) {
		price := math.NaN()
		switch tickType {
		case 0, 69: // bid size
			t.PrevBidSize = t.BidSize
			t.BidSize = size
			price = t.Bid
		case 3, 70: // ask size
			t.PrevAskSize = t.AskSize
			t.AskSize = size
			price = t.Ask
		case 5, 71: // last size
			t.PrevLastSize = t.LastSize
			t.LastSize = size
			price = t.Last
		default:
			if field, ok := state.SizeTickMap[tickType]; ok {
				setTickerFloat(t, field, size)
			}
		}
		t.Ticks = append(t.Ticks, market.TickData{Time: ib.now(), TickType: tickType, Price: price, Size: size})
	})
}

func (ib *IB) handleTickGeneric(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	value := r.ReadFloat()

	ib.updateTicker(reqID, func(t *market.Ticker) {
		if field, ok := state.GenericTickMap[tickType]; ok {
			setTickerFloat(t, field, value)
		}
	})
}

func (ib *IB) handleTickString(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	value := r.ReadString()

	ib.updateTicker(reqID, func(t *market.Ticker) {
		switch tickType {
		case 45, 88: // last timestamp, delayed last timestamp
			processTimestampTick(t, tickType, value)
		case 48, 77: // RT volume, RT trade volume
			ib.processRtVolumeTick(t, tickType, value)
		case 59: // dividends: "past12,next12,nextDate,nextAmount"
			ib.processDividendTick(t, value)
		case 47: // fundamental ratios: "key=value;key=value;..."
			ib.processFundamentalRatios(t, value)
		default:
			if field, ok := state.StringTickMap[tickType]; ok {
				setTickerString(t, field, value)
			}
		}
	})
}

func processTimestampTick(t *market.Ticker, tickType int, value string) {
	ts, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ts == 0 {
		return
	}
	tm := time.Unix(ts, 0).UTC()
	switch tickType {
	case 45:
		t.LastTimestamp = tm
	case 88:
		t.DelayedLastTimestamp = tm
	}
}

func (ib *IB) processRtVolumeTick(t *market.Ticker, tickType int, value string) {
	// Format: price;size;ms_since_epoch;total_volume;VWAP;single_trade
	parts := strings.Split(value, ";")
	if len(parts) < 6 {
		return
	}
	priceStr, sizeStr, rtTimeStr, volumeStr, vwapStr := parts[0], parts[1], parts[2], parts[3], parts[4]

	if v, err := strconv.ParseFloat(volumeStr, 64); err == nil {
		switch tickType {
		case 48:
			t.RTVolume = v
		case 77:
			t.RTTradeVolume = v
		}
	}
	if v, err := strconv.ParseFloat(vwapStr, 64); err == nil {
		t.VWAP = v
	}
	if ms, err := strconv.ParseInt(rtTimeStr, 10, 64); err == nil {
		t.RTTime = time.UnixMilli(ms).UTC()
	}
	price, err1 := strconv.ParseFloat(priceStr, 64)
	size, err2 := strconv.ParseFloat(sizeStr, 64)
	if err1 == nil && err2 == nil {
		t.PrevLast, t.PrevLastSize = t.Last, t.LastSize
		t.Last, t.LastSize = price, size
		t.Ticks = append(t.Ticks, market.TickData{Time: ib.now(), TickType: tickType, Price: price, Size: size})
	}
}

// optionalGreek decodes one tickOptionComputation value; TWS sends sentinel
// values (-1 or -2 depending on the field) for "not computed".
func optionalGreek(s, sentinel string) *float64 {
	if s == "" || s == sentinel {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v == UnsetDouble || math.IsInf(v, 0) || math.IsNaN(v) {
		return nil
	}
	return &v
}

// handleTickOptionComputation decodes option greeks (server version >= 156:
// no version field, tickAttrib present).
func (ib *IB) handleTickOptionComputation(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	comp := &market.OptionComputation{TickAttrib: int(r.ReadInt())}
	comp.ImpliedVol = optionalGreek(r.ReadString(), "-1")
	comp.Delta = optionalGreek(r.ReadString(), "-2")
	comp.OptPrice = optionalGreek(r.ReadString(), "-1")
	comp.PvDividend = optionalGreek(r.ReadString(), "-1")
	comp.Gamma = optionalGreek(r.ReadString(), "-2")
	comp.Vega = optionalGreek(r.ReadString(), "-2")
	comp.Theta = optionalGreek(r.ReadString(), "-2")
	comp.UndPrice = optionalGreek(r.ReadString(), "-1")

	field, ok := state.GreeksTickMap[tickType]
	if !ok {
		return
	}
	ib.updateTicker(reqID, func(t *market.Ticker) {
		switch field {
		case "BidGreeks":
			t.BidGreeks = comp
		case "AskGreeks":
			t.AskGreeks = comp
		case "LastGreeks":
			t.LastGreeks = comp
		case "ModelGreeks":
			t.ModelGreeks = comp
		case "CustGreeks":
			t.CustGreeks = comp
		}
	})
}

func (ib *IB) handleTickEFP(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	efp := &market.EfpData{
		BasisPoints:              r.ReadFloat(),
		FormattedBasisPoints:     r.ReadString(),
		ImpliedFuture:            r.ReadFloat(),
		HoldDays:                 int(r.ReadInt()),
		FutureLastTradeDate:      r.ReadString(),
		DividendImpact:           r.ReadFloat(),
		DividendsToLastTradeDate: r.ReadFloat(),
	}
	field, ok := state.EfpTickMap[tickType]
	if !ok {
		return
	}
	ib.updateTicker(reqID, func(t *market.Ticker) {
		switch field {
		case "BidEfp":
			t.BidEfp = efp
		case "AskEfp":
			t.AskEfp = efp
		case "LastEfp":
			t.LastEfp = efp
		case "OpenEfp":
			t.OpenEfp = efp
		case "HighEfp":
			t.HighEfp = efp
		case "LowEfp":
			t.LowEfp = efp
		case "CloseEfp":
			t.CloseEfp = efp
		}
	})
}

// handleTickReqParams: reqId, minTick, bboExchange, snapshotPermissions.
func (ib *IB) handleTickReqParams(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	minTick := r.ReadFloat()
	bbo := r.ReadString()
	perms := int(r.ReadInt())
	ib.updateTicker(reqID, func(t *market.Ticker) {
		t.MinTick = minTick
		t.BboExchange = bbo
		t.SnapshotPermissions = perms
	})
}

func (ib *IB) handleMarketDataType(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	mdType := int(r.ReadInt())
	ib.updateTicker(reqID, func(t *market.Ticker) {
		t.MarketDataType = mdType
	})
}

// handleTickSnapshotEnd marks a snapshot request complete. The request is
// released and completed in flushSnapshotEnds, after this batch's ticks are
// applied to its ticker.
func (ib *IB) handleTickSnapshotEnd(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	ib.snapshots.end(reqID)
}

// handleTickByTick: reqId, tickType, time, then per type:
// 1/2 (Last/AllLast): price, size, mask, exchange, specialConditions
// 3 (BidAsk): bidPrice, askPrice, bidSize, askSize, mask
// 4 (MidPoint): midPoint
func (ib *IB) handleTickByTick(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	tm := time.Unix(r.ReadInt(), 0).UTC()

	switch tickType {
	case 1, 2:
		price := r.ReadFloat()
		size := r.ReadFloat()
		mask := r.ReadInt()
		tick := market.TickByTickAllLast{
			TickType: tickType, Time: tm, Price: price, Size: size,
			TickAttribLast:    market.TickAttribLast{PastLimit: mask&1 != 0, Unreported: mask&2 != 0},
			Exchange:          r.ReadString(),
			SpecialConditions: r.ReadString(),
		}
		ib.updateTicker(reqID, func(t *market.Ticker) { applyTickByTickLast(t, tick) })
	case 3:
		tick := market.TickByTickBidAsk{Time: tm, BidPrice: r.ReadFloat(), AskPrice: r.ReadFloat(), BidSize: r.ReadFloat(), AskSize: r.ReadFloat()}
		mask := r.ReadInt()
		tick.TickAttribBidAsk = market.TickAttribBidAsk{BidPastLow: mask&1 != 0, AskPastHigh: mask&2 != 0}
		ib.updateTicker(reqID, func(t *market.Ticker) { applyTickByTickBidAsk(t, tick) })
	case 4:
		tick := market.TickByTickMidPoint{Time: tm, MidPoint: r.ReadFloat()}
		ib.updateTicker(reqID, func(t *market.Ticker) { applyTickByTickMidPoint(t, tick) })
	}
}

// Tick-by-tick updates (live or backfill), applied under the ticker lock.

func applyTickByTickLast(t *market.Ticker, tick market.TickByTickAllLast) {
	t.PrevLast, t.PrevLastSize = t.Last, t.LastSize
	t.Last, t.LastSize = tick.Price, tick.Size
	t.TickByTicks = append(t.TickByTicks, tick)
}

func applyTickByTickBidAsk(t *market.Ticker, tick market.TickByTickBidAsk) {
	if tick.BidPrice != t.Bid || tick.BidSize != t.BidSize {
		t.PrevBid, t.PrevBidSize = t.Bid, t.BidSize
		t.Bid, t.BidSize = tick.BidPrice, tick.BidSize
	}
	if tick.AskPrice != t.Ask || tick.AskSize != t.AskSize {
		t.PrevAsk, t.PrevAskSize = t.Ask, t.AskSize
		t.Ask, t.AskSize = tick.AskPrice, tick.AskSize
	}
	t.TickByTicks = append(t.TickByTicks, tick)
}

func applyTickByTickMidPoint(t *market.Ticker, tick market.TickByTickMidPoint) {
	t.TickByTicks = append(t.TickByTicks, tick)
}

// handleMktDepth: version, reqId, position, operation, side, price, size.
func (ib *IB) handleMktDepth(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	position := int(r.ReadInt())
	operation := int(r.ReadInt())
	side := int(r.ReadInt())
	price := r.ReadFloat()
	size := r.ReadFloat()
	ib.applyDepth(reqID, position, "", operation, side, price, size)
}

// handleMktDepthL2: version, reqId, position, marketMaker, operation, side,
// price, size, isSmartDepth.
func (ib *IB) handleMktDepthL2(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	position := int(r.ReadInt())
	marketMaker := r.ReadString()
	operation := int(r.ReadInt())
	side := int(r.ReadInt())
	price := r.ReadFloat()
	size := r.ReadFloat()
	ib.applyDepth(reqID, position, marketMaker, operation, side, price, size)
}

// resetDepth empties the order book of a depth request (error 317: TWS
// resets the book and re-sends it).
func (ib *IB) resetDepth(reqID int64) {
	ib.updateTicker(reqID, func(t *market.Ticker) {
		t.DomBids, t.DomAsks = nil, nil
	})
}

// applyDepth updates the order book: operation 0 insert, 1 update,
// 2 delete; side 0 ask, 1 bid.
func (ib *IB) applyDepth(reqID int64, position int, marketMaker string, operation, side int, price, size float64) {
	if position < 0 {
		return
	}
	fresh := ib.takeDepthFresh(reqID)
	ib.updateTicker(reqID, func(t *market.Ticker) {
		if fresh {
			// First update of a (re)request: drop the previous book.
			t.DomBids, t.DomAsks = nil, nil
		}
		dom := &t.DomAsks
		if side == 1 {
			dom = &t.DomBids
		}
		level := market.DOMLevel{Price: price, Size: size, MarketMaker: marketMaker}
		switch operation {
		case 0:
			if position >= len(*dom) {
				*dom = append(*dom, level)
			} else {
				*dom = append(*dom, market.DOMLevel{})
				copy((*dom)[position+1:], (*dom)[position:])
				(*dom)[position] = level
			}
		case 1:
			if position < len(*dom) {
				(*dom)[position] = level
			} else {
				*dom = append(*dom, level)
			}
		case 2:
			if position < len(*dom) {
				price = (*dom)[position].Price
				size = 0
				*dom = append((*dom)[:position], (*dom)[position+1:]...)
			}
		}
		t.DomTicks = append(t.DomTicks, market.MktDepthData{
			Time: ib.now(), Position: position, MarketMaker: marketMaker,
			Operation: operation, Side: side, Price: price, Size: size,
		})
	})
}
