package state

import (
	"sync"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

// OrderKey uniquely identifies a trade: (clientID, orderID) for API orders, permID for manual.
type OrderKey struct {
	ClientID int64
	OrderID  int64
}

// Manager holds all connection state, organized by domain.
// Every map in the Manager is guarded by Mu: the reader goroutine mutates it
// while user goroutines start requests and read snapshots concurrently.
type Manager struct {
	Mu sync.RWMutex

	// Domain-specific state
	OrderState
	AccountState
	MarketState

	// Request-response tracking
	Requests       *RequestMap
	Results        map[int64]interface{}
	ReqID2Contract map[int64]*contract.Contract

	// News
	NewsBulletins map[int]*account.NewsBulletin

	// Timing (guarded by Mu; use SetLastTime/GetLastTime)
	LastTime  time.Time
	TimeFloat float64 // time.Time as float (for fast timestamp)

	// Accounts
	Accounts []string
	ClientID int64
}

// NewManager creates a new state Manager with initialized maps.
func NewManager() *Manager {
	return &Manager{
		OrderState:     newOrderState(),
		AccountState:   newAccountState(),
		MarketState:    newMarketState(),
		Requests:       NewRequestMap(),
		Results:        make(map[int64]interface{}),
		ReqID2Contract: make(map[int64]*contract.Contract),
		NewsBulletins:  make(map[int]*account.NewsBulletin),
	}
}

// StartReq begins tracking a request-response cycle.
func (m *Manager) StartReq(reqID int64, con *contract.Contract) <-chan Result {
	m.Mu.Lock()
	if con != nil {
		m.ReqID2Contract[reqID] = con
	}
	m.Results[reqID] = nil
	m.Mu.Unlock()
	return m.Requests.Start(reqID)
}

// CancelReq abandons a pending request (e.g. on context cancellation) and
// drops any partial results accumulated for it.
func (m *Manager) CancelReq(reqID int64) {
	m.Mu.Lock()
	delete(m.ReqID2Contract, reqID)
	delete(m.Results, reqID)
	m.Mu.Unlock()
	m.Requests.Cancel(reqID)
}

// EndReq completes a request-response cycle.
func (m *Manager) EndReq(reqID int64, value interface{}, err error) {
	m.Mu.Lock()
	delete(m.ReqID2Contract, reqID)
	result := m.Results[reqID]
	delete(m.Results, reqID)
	m.Mu.Unlock()

	if value == nil {
		value = result
	}
	m.Requests.Complete(reqID, Result{Value: value, Err: err})
}

// AppendResult accumulates a partial result for a multi-message response.
func (m *Manager) AppendResult(reqID int64, item interface{}) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	existing, ok := m.Results[reqID]
	if !ok {
		return
	}
	if existing == nil {
		m.Results[reqID] = []interface{}{item}
	} else if list, ok := existing.([]interface{}); ok {
		m.Results[reqID] = append(list, item)
	}
}

// HasResult reports whether a request with the given key is collecting results.
func (m *Manager) HasResult(reqID int64) bool {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	_, ok := m.Results[reqID]
	return ok
}

// SetLastTime records the arrival time of the current network batch.
func (m *Manager) SetLastTime(t time.Time) {
	m.Mu.Lock()
	m.LastTime = t
	m.TimeFloat = float64(t.UnixMilli()) / 1000.0
	m.Mu.Unlock()
}

// GetLastTime returns the arrival time of the current network batch.
func (m *Manager) GetLastTime() (time.Time, float64) {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	return m.LastTime, m.TimeFloat
}

// --- Read-only accessors (thread-safe) ---

// GetPositions returns a snapshot of all positions.
func (m *Manager) GetPositions() []account.Position {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	var result []account.Position
	for _, acctPositions := range m.Positions {
		for _, p := range acctPositions {
			result = append(result, *p)
		}
	}
	return result
}

// GetTrades returns a snapshot of all trades.
func (m *Manager) GetTrades() []*order.Trade {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	var result []*order.Trade
	for _, t := range m.Trades {
		result = append(result, t)
	}
	return result
}

// GetOpenTrades returns trades that are still active.
func (m *Manager) GetOpenTrades() []*order.Trade {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	var result []*order.Trade
	for _, t := range m.Trades {
		if !t.IsDone() {
			result = append(result, t)
		}
	}
	return result
}

// GetFills returns a snapshot of all fills.
func (m *Manager) GetFills() []order.Fill {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	var result []order.Fill
	for _, f := range m.Fills {
		result = append(result, *f)
	}
	return result
}

// GetAccountValues returns a snapshot of account values.
func (m *Manager) GetAccountValues() []account.AccountValue {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	var result []account.AccountValue
	for _, v := range m.AccountValues {
		result = append(result, *v)
	}
	return result
}

// GetPortfolio returns portfolio items for an account.
func (m *Manager) GetPortfolio(acct string) []account.PortfolioItem {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	var result []account.PortfolioItem
	if items, ok := m.Portfolio[acct]; ok {
		for _, item := range items {
			result = append(result, *item)
		}
	}
	return result
}

// GetPendingTickers returns tickers with pending updates.
func (m *Manager) GetPendingTickers() []*market.Ticker {
	m.Mu.RLock()
	defer m.Mu.RUnlock()
	result := make([]*market.Ticker, 0, len(m.PendingTickers))
	for t := range m.PendingTickers {
		result = append(result, t)
	}
	return result
}

// MarkTickerPending marks a ticker as having pending updates.
func (m *Manager) MarkTickerPending(t *market.Ticker) {
	m.Mu.Lock()
	m.PendingTickers[t] = true
	m.Mu.Unlock()
}

// ClearPendingTickers clears pending tickers and their per-batch tick lists.
//
// The pending set is swapped out under Mu but the tickers are cleared after
// releasing it, so Mu is never held while waiting for a ticker lock.
func (m *Manager) ClearPendingTickers() {
	m.Mu.Lock()
	pending := m.PendingTickers
	m.PendingTickers = make(map[*market.Ticker]bool)
	m.Mu.Unlock()
	clearTickLists(pending)
}

// ResetSession clears per-connection state (market data subscriptions,
// pending request results) but keeps orders, fills, positions and account
// values so that Trade objects held by callers survive a reconnect and can be
// re-synchronised with the broker.
func (m *Manager) ResetSession() {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	m.MarketState.reset()
	m.Results = make(map[int64]interface{})
	m.ReqID2Contract = make(map[int64]*contract.Contract)
}

// Reset clears all state (called on explicit disconnect).
func (m *Manager) Reset() {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	m.OrderState.reset()
	m.AccountState.reset()
	m.MarketState.reset()
	m.Results = make(map[int64]interface{})
	m.ReqID2Contract = make(map[int64]*contract.Contract)
	m.NewsBulletins = make(map[int]*account.NewsBulletin)
	m.Accounts = nil
}
