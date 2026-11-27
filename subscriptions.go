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
	// epoch is the connection (IB.connEpoch) the subscription was last
	// sent on, 0 if that send failed. Guarded by IB.subsMu.
	epoch int64
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

func (ib *IB) registerSub(reqID int64, s *subscription, epoch int64) {
	if s.ticker != nil {
		ib.registerTickerReq(reqID, s.con, s.ticker)
	}
	if s.target != nil {
		ib.state.Mu.Lock()
		ib.state.ReqID2Subscriber[reqID] = s.target
		ib.state.Mu.Unlock()
	}
	ib.subsMu.Lock()
	s.epoch = epoch
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
	ib.registerSub(reqID, s, ib.connEpoch.Load())
	if s.onRestart != nil {
		s.onRestart(reqID)
	}
	if err := s.send(reqID); err != nil {
		ib.unregisterSub(reqID)
		return 0, err
	}
	return reqID, nil
}

// endSub cancels the first subscription matching pred: the lowest request
// id, preferring subscriptions live on the current connection. It is
// serialised with restoreSubscriptions, so a cancelled subscription is
// never re-sent.
func (ib *IB) endSub(pred func(*subscription) bool) bool {
	ib.subOpMu.Lock()
	defer ib.subOpMu.Unlock()

	cur := ib.connEpoch.Load()
	ib.subsMu.Lock()
	var reqID int64
	var s *subscription
	for id, cand := range ib.subs {
		if !pred(cand) {
			continue
		}
		if s == nil {
			reqID, s = id, cand
			continue
		}
		candLive, bestLive := cand.epoch == cur, s.epoch == cur
		if candLive != bestLive {
			if candLive {
				reqID, s = id, cand
			}
		} else if id < reqID {
			reqID, s = id, cand
		}
	}
	if s == nil {
		ib.subsMu.Unlock()
		return false
	}
	delete(ib.subs, reqID)
	live := s.epoch != 0 && s.epoch == cur
	ib.subsMu.Unlock()

	ib.releaseReq(reqID)
	// A subscription not (re)sent on the current connection is unknown to
	// TWS: there is nothing to cancel.
	if live && s.cancel != nil {
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

// restoreSubscriptions re-sends, with new request ids, every subscription
// that was sent on an earlier connection (or whose re-send failed).
// Subscriptions already made on the current connection, e.g. from a
// ConnectedEvent handler, are left alone. A subscription whose re-send
// fails stays registered and is retried after the next reconnect. Market
// data type and account updates are restored first so the re-requested
// data arrives in the expected form.
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

	cur := ib.connEpoch.Load()
	ib.subsMu.Lock()
	var ids []int64
	for id, s := range ib.subs {
		if s.epoch != cur {
			ids = append(ids, id)
		}
	}
	ib.subsMu.Unlock()
	slices.Sort(ids)

	restored, failed := 0, 0
	var firstErr error
	for _, id := range ids {
		ok, err := ib.restoreSub(id, cur)
		switch {
		case err != nil:
			failed++
			if firstErr == nil {
				firstErr = err
			}
		case ok:
			restored++
		}
	}
	if restored > 0 {
		log.Printf("ibgo: restored %d subscriptions", restored)
	}
	if failed > 0 {
		log.Printf("ibgo: %d subscriptions not restored, retrying after the next reconnect: %v", failed, firstErr)
	}
}

// restoreSub re-sends one subscription of an earlier connection under a new
// request id. It reports false when the subscription was cancelled
// meanwhile; on a send error the subscription stays registered, marked as
// not sent, for the next reconnect.
func (ib *IB) restoreSub(oldID, epoch int64) (bool, error) {
	ib.subOpMu.Lock()
	defer ib.subOpMu.Unlock()

	ib.subsMu.Lock()
	s := ib.subs[oldID]
	if s == nil || s.epoch == epoch {
		ib.subsMu.Unlock()
		return false, nil
	}
	delete(ib.subs, oldID)
	ib.subsMu.Unlock()
	ib.releaseReq(oldID)

	reqID := ib.client.GetReqID()
	ib.registerSub(reqID, s, epoch)
	if s.onRestart != nil {
		s.onRestart(reqID)
	}
	if err := s.send(reqID); err != nil {
		ib.subsMu.Lock()
		s.epoch = 0
		ib.subsMu.Unlock()
		return false, err
	}
	return true, nil
}
