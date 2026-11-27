package ibgo

// Registry of streaming subscriptions (market data, tick-by-tick, depth,
// real-time bars, keep-up-to-date bars, PnL). It lets Cancel* find the
// request id of a subscription and lets ConnectWithReconnect re-request
// every live subscription after a reconnect, so tickers and bar lists held
// by the application keep updating.

import (
	"log"
	"slices"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
)

type subKind int

const (
	subMktData subKind = iota + 1
	subTickByTick
	subMktDepth
	subRealTimeBars
	subHistoricalBars
	subPnL
	subPnLSingle
)

type subscription struct {
	kind   subKind
	con    *contract.Contract
	ticker *market.Ticker // tick subscriptions
	target interface{}    // ReqID2Subscriber value (bar lists, PnL keys)
	tag    string         // kind-specific discriminator (tick type, smart depth)
	send   func(reqID int64) error
	cancel func(reqID int64) error
	// onRestart is called with the new request id after a reconnect.
	onRestart func(reqID int64)
}

// tickerForContract returns the ticker already used for the contract (one
// ticker per qualified contract, like ib_async) or a new one.
func (ib *IB) tickerForContract(con *contract.Contract) *market.Ticker {
	if key := con.Key(); key != 0 {
		ib.state.Mu.RLock()
		t := ib.state.Tickers[key]
		ib.state.Mu.RUnlock()
		if t != nil {
			return t
		}
	}
	return market.NewTicker(con)
}

// registerTickerReq maps a market data request id to its ticker.
func (ib *IB) registerTickerReq(reqID int64, con *contract.Contract, t *market.Ticker) {
	ib.state.Mu.Lock()
	ib.state.ReqID2Ticker[reqID] = t
	ib.state.ReqID2Contract[reqID] = con
	if key := con.Key(); key != 0 {
		ib.state.Tickers[key] = t
	}
	ib.state.Mu.Unlock()
}

// releaseReq drops the request-id mappings of a streaming request. The
// contract's ticker stays registered while other requests still use it.
func (ib *IB) releaseReq(reqID int64) {
	ib.state.Mu.Lock()
	defer ib.state.Mu.Unlock()
	t := ib.state.ReqID2Ticker[reqID]
	delete(ib.state.ReqID2Ticker, reqID)
	delete(ib.state.ReqID2Contract, reqID)
	delete(ib.state.ReqID2Subscriber, reqID)
	if t == nil {
		return
	}
	key := t.Contract.Key()
	if key == 0 || ib.state.Tickers[key] != t {
		return
	}
	for _, other := range ib.state.ReqID2Ticker {
		if other == t {
			return
		}
	}
	delete(ib.state.Tickers, key)
}

func (ib *IB) registerSub(reqID int64, s *subscription) {
	if s.ticker != nil {
		ib.registerTickerReq(reqID, s.con, s.ticker)
	}
	if s.target != nil {
		ib.state.Mu.Lock()
		ib.state.ReqID2Subscriber[reqID] = s.target
		ib.state.Mu.Unlock()
	}
	ib.subsMu.Lock()
	ib.subs[reqID] = s
	ib.subsMu.Unlock()
}

func (ib *IB) unregisterSub(reqID int64) *subscription {
	ib.subsMu.Lock()
	s := ib.subs[reqID]
	delete(ib.subs, reqID)
	ib.subsMu.Unlock()
	ib.releaseReq(reqID)
	return s
}

// startSub registers a subscription under a new request id and sends it.
func (ib *IB) startSub(s *subscription) (int64, error) {
	reqID := ib.client.GetReqID()
	ib.registerSub(reqID, s)
	if s.onRestart != nil {
		s.onRestart(reqID)
	}
	if err := s.send(reqID); err != nil {
		ib.unregisterSub(reqID)
		return 0, err
	}
	return reqID, nil
}

// endSub cancels the first subscription matching pred (lowest request id).
func (ib *IB) endSub(pred func(*subscription) bool) bool {
	ib.subsMu.Lock()
	var ids []int64
	for id, s := range ib.subs {
		if pred(s) {
			ids = append(ids, id)
		}
	}
	ib.subsMu.Unlock()
	if len(ids) == 0 {
		return false
	}
	reqID := slices.Min(ids)
	s := ib.unregisterSub(reqID)
	if s != nil && s.cancel != nil {
		_ = s.cancel(reqID)
	}
	return true
}

// endSubByContract cancels a subscription of kind for con, matching the
// contract pointer first and then the contract key (conId).
func (ib *IB) endSubByContract(kind subKind, tag string, con *contract.Contract) bool {
	if ib.endSub(func(s *subscription) bool { return s.kind == kind && s.tag == tag && s.con == con }) {
		return true
	}
	key := con.Key()
	if key == 0 {
		return false
	}
	return ib.endSub(func(s *subscription) bool { return s.kind == kind && s.tag == tag && s.con.Key() == key })
}

// clearSubs forgets all subscriptions (explicit Disconnect).
func (ib *IB) clearSubs() {
	ib.subsMu.Lock()
	ib.subs = map[int64]*subscription{}
	ib.subsMu.Unlock()
	ib.acctMu.Lock()
	ib.acctUpdates = nil
	ib.acctMu.Unlock()
	ib.pnlMu.Lock()
	clear(ib.pnl)
	clear(ib.pnlSingle)
	ib.pnlMu.Unlock()
}

// releaseFailedSnapshot drops the ticker mapping of a failed one-off
// (snapshot) request; streaming subscriptions stay registered.
func (ib *IB) releaseFailedSnapshot(reqID int64) {
	ib.subsMu.Lock()
	_, isSub := ib.subs[reqID]
	ib.subsMu.Unlock()
	if !isSub && ib.tickerFor(reqID) != nil {
		ib.releaseReq(reqID)
	}
}

// restoreSubscriptions re-sends every live subscription with new request
// ids after a reconnect. Market data type and account updates are restored
// first so the re-requested data arrives in the expected form.
func (ib *IB) restoreSubscriptions() {
	if t := ib.mdType.Load(); t != 0 {
		if err := ib.client.ReqMarketDataType(int(t)); err != nil {
			log.Printf("ibgo: restore market data type: %v", err)
		}
	}
	ib.acctMu.Lock()
	acct := ib.acctUpdates
	ib.acctMu.Unlock()
	if acct != nil {
		if err := ib.client.ReqAccountUpdates(true, *acct); err != nil {
			log.Printf("ibgo: restore account updates: %v", err)
		}
	}

	ib.subsMu.Lock()
	old := ib.subs
	ib.subs = map[int64]*subscription{}
	ib.subsMu.Unlock()

	ids := make([]int64, 0, len(old))
	for id := range old {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		ib.releaseReq(id)
		if _, err := ib.startSub(old[id]); err != nil {
			log.Printf("ibgo: restore subscription (reqId %d): %v", id, err)
		}
	}
	if len(ids) > 0 {
		log.Printf("ibgo: restored %d subscriptions", len(ids))
	}
}
