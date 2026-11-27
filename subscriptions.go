package ibgo

// Registry of streaming subscriptions (market data, tick-by-tick, depth,
// real-time bars, keep-up-to-date bars, PnL). It lets Cancel* find the
// request id of a subscription and lets ConnectWithReconnect re-request
// every live subscription after a reconnect, so tickers and bar lists held
// by the application keep updating.

import (
	"errors"
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
	kind subKind
	con  *contract.Contract
	// ticker of a tick subscription (subMktData, subTickByTick,
	// subMktDepth): the contract's shared ticker, set when the
	// subscription is first registered unless given.
	ticker *market.Ticker
	target interface{} // ReqID2Subscriber value (bar lists, PnL keys)
	tag    string      // kind-specific discriminator (tick type, smart depth)
	send   func(reqID int64) error
	cancel func(reqID int64) error
	// onSend is called with the request id before every send, while
	// subOpMu is held: it may only touch internal state, never a lock an
	// application can hold (ticker, bar list).
	onSend func(reqID int64)
	// setReqID publishes a new request id on the returned object (bar
	// lists). After a reconnect it runs once subOpMu is released.
	setReqID func(reqID int64)
	// epoch is the connection (IB.connEpoch) the subscription was last
	// sent on, 0 if that send failed. Guarded by IB.subsMu.
	epoch int64
}

func (s *subscription) usesTicker() bool {
	return s.kind == subMktData || s.kind == subTickByTick || s.kind == subMktDepth
}

// ErrDepthSubscribed is returned by ReqMktDepth when the contract already
// has a market depth subscription: its ticker holds a single order book.
var ErrDepthSubscribed = errors.New("ibgo: market depth already subscribed for this contract (cancel it first)")

// Lock order: subOpMu, then subsMu / depthMu, then state.Mu. Ticker and
// bar-list locks are never taken while holding subOpMu: applications may
// hold their read locks while calling Cancel*.

// acquireTicker maps reqID to the ticker already used for the contract
// (one ticker per qualified contract, like ib_async) or to a new one, under
// one lock so concurrent requests for a contract cannot create two.
func (ib *IB) acquireTicker(reqID int64, con *contract.Contract) *market.Ticker {
	ib.state.Mu.Lock()
	defer ib.state.Mu.Unlock()
	key := con.Key()
	t := ib.state.Tickers[key]
	if key == 0 || t == nil {
		t = market.NewTicker(con)
		if key != 0 {
			ib.state.Tickers[key] = t
		}
	}
	ib.state.ReqID2Ticker[reqID] = t
	ib.state.ReqID2Contract[reqID] = con
	return t
}

// registerTickerReq maps a request id to a given ticker.
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
	if s.usesTicker() {
		if s.ticker == nil {
			s.ticker = ib.acquireTicker(reqID, s.con)
		} else {
			ib.registerTickerReq(reqID, s.con, s.ticker)
		}
	}
	ib.state.Mu.Lock()
	if s.con != nil {
		// Lets errors of the request carry its contract (IBError.Contract).
		ib.state.ReqID2Contract[reqID] = s.con
	}
	if s.target != nil {
		ib.state.ReqID2Subscriber[reqID] = s.target
	}
	ib.state.Mu.Unlock()
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
	ib.depthDone(reqID)
	ib.releaseReq(reqID)
	return s
}

// startSub registers a subscription under a new request id and sends it.
// Registering and sending are serialised with cancelling (subOpMu), so a
// concurrent Cancel* either finds nothing or cancels after the send.
func (ib *IB) startSub(s *subscription) (int64, error) {
	ib.subOpMu.Lock()
	defer ib.subOpMu.Unlock()
	if s.kind == subMktDepth && ib.hasSub(func(o *subscription) bool {
		return o.kind == subMktDepth && sameContract(o.con, s.con)
	}) {
		return 0, ErrDepthSubscribed
	}
	reqID := ib.client.GetReqID()
	ib.registerSub(reqID, s, ib.connEpoch.Load())
	if s.setReqID != nil {
		s.setReqID(reqID) // the object is not returned yet: nobody holds its lock
	}
	if s.onSend != nil {
		s.onSend(reqID)
	}
	if err := s.send(reqID); err != nil {
		ib.unregisterSub(reqID)
		return 0, err
	}
	return reqID, nil
}

func (ib *IB) hasSub(pred func(*subscription) bool) bool {
	ib.subsMu.Lock()
	defer ib.subsMu.Unlock()
	for _, s := range ib.subs {
		if pred(s) {
			return true
		}
	}
	return false
}

// sameContract reports whether a and b denote the same contract (and so
// share a ticker): the same pointer or the same contract key.
func sameContract(a, b *contract.Contract) bool {
	if a == b {
		return true
	}
	k := a.Key()
	return k != 0 && k == b.Key()
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

	ib.depthDone(reqID)
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
	ib.depthMu.Lock()
	clear(ib.depthFresh)
	ib.depthMu.Unlock()
	ib.acctMu.Lock()
	ib.acctUpdates = nil
	ib.acctMu.Unlock()
	ib.dlMu.Lock()
	ib.acctDL = nil
	ib.dlMu.Unlock()
	ib.pnlMu.Lock()
	clear(ib.pnl)
	clear(ib.pnlSingle)
	ib.pnlMu.Unlock()
}

// resetSession drops the per-connection state after the connection was
// lost, keeping the request-id mappings of the registered subscriptions
// (and with them the one ticker per contract) until restoreSubscriptions
// moves each to a new request id. Request ids are never reused, so nothing
// arrives for the old ones.
func (ib *IB) resetSession() {
	ib.subsMu.Lock()
	keep := make(map[int64]bool, len(ib.subs))
	for id := range ib.subs {
		keep[id] = true
	}
	ib.subsMu.Unlock()
	ib.state.ResetSession(keep)
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
// fails stays registered and is retried after the next reconnect. Account
// updates are restored first; the market data type was already re-applied
// by Connect.
func (ib *IB) restoreSubscriptions() {
	ib.acctMu.Lock()
	if acct := ib.acctUpdates; acct != nil {
		ib.beginAcctDownload(*acct)
		if err := ib.client.ReqAccountUpdates(true, *acct); err != nil {
			log.Printf("ibgo: restore account updates: %v", err)
		}
	}
	ib.acctMu.Unlock()

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
	var publish []func()
	for _, id := range ids {
		ok, pub, err := ib.restoreSub(id, cur)
		if pub != nil {
			publish = append(publish, pub)
		}
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
	// New request ids are published on the returned objects after every
	// subscription was re-sent: this takes their locks, which the
	// application may hold for a while.
	for _, pub := range publish {
		pub()
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
// not sent, for the next reconnect. The returned func, if any, publishes
// the new request id on the subscription's object; call it without locks.
func (ib *IB) restoreSub(oldID, epoch int64) (bool, func(), error) {
	ib.subOpMu.Lock()
	ib.subsMu.Lock()
	s := ib.subs[oldID]
	if s == nil || s.epoch == epoch {
		ib.subsMu.Unlock()
		ib.subOpMu.Unlock()
		return false, nil, nil
	}
	delete(ib.subs, oldID)
	ib.subsMu.Unlock()

	// Register the new id before releasing the old one, so the contract's
	// ticker stays the registered one throughout.
	reqID := ib.client.GetReqID()
	ib.registerSub(reqID, s, epoch)
	ib.depthDone(oldID)
	ib.releaseReq(oldID)
	if s.onSend != nil {
		s.onSend(reqID)
	}
	err := s.send(reqID)
	if err != nil {
		ib.subsMu.Lock()
		s.epoch = 0
		ib.subsMu.Unlock()
	}
	ib.subOpMu.Unlock()

	// Not under subOpMu: setReqID takes the lock of an object the
	// application holds and may be reading while it calls Cancel*.
	var publish func()
	if s.setReqID != nil {
		publish = func() { s.setReqID(reqID) }
	}
	return err == nil, publish, err
}

// --- market depth books ---

// depthStarted marks a depth request whose ticker book must be emptied
// before its first update is applied (on the reader goroutine, which owns
// the book: callbacks may read DomBids/DomAsks without locking).
func (ib *IB) depthStarted(reqID int64) {
	ib.depthMu.Lock()
	ib.depthFresh[reqID] = true
	ib.depthMu.Unlock()
}

func (ib *IB) depthDone(reqID int64) {
	ib.depthMu.Lock()
	delete(ib.depthFresh, reqID)
	ib.depthMu.Unlock()
}

// takeDepthFresh reports (once) whether reqID's book must be emptied.
func (ib *IB) takeDepthFresh(reqID int64) bool {
	ib.depthMu.Lock()
	defer ib.depthMu.Unlock()
	fresh := ib.depthFresh[reqID]
	delete(ib.depthFresh, reqID)
	return fresh
}
