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

// Manager holds all connection state, mirroring Python's Wrapper class.
// All mutations happen on the reader goroutine; reads use RWMutex.
type Manager struct {
	Mu sync.RWMutex

	// Account data
	AccountValues map[string]*account.AccountValue // "account:tag:currency:modelCode" → value
	Portfolio     map[string]map[int64]*account.PortfolioItem // account → conId → item
	Positions     map[string]map[int64]*account.Position      // account → conId → position

	// Order tracking
	Trades       map[OrderKey]*order.Trade // (clientID, orderID) → trade
	PermID2Trade map[int64]*order.Trade    // permID → trade
	Fills        map[string]*order.Fill    // execID → fill

	// Market data
	Tickers        map[int64]*market.Ticker  // contract key → ticker
	PendingTickers map[*market.Ticker]bool
	ReqID2Ticker   map[int64]*market.Ticker  // reqID → ticker

	// Subscriptions
	ReqID2Subscriber map[int64]interface{} // reqID → BarDataList or ScanDataList

	// PnL
	ReqID2PnL       map[int64]*account.PnL
	ReqID2PnLSingle map[int64]*account.PnLSingle

	// Request-response tracking
	Requests       *RequestMap
	Results        map[int64]interface{} // reqID → accumulated results
	ReqID2Contract map[int64]*contract.Contract

	// News
	NewsBulletins map[int]*account.NewsBulletin

	// Timing
	LastTime time.Time

	// Accounts
	Accounts []string
	ClientID int64
}

// NewManager creates a new state Manager with initialized maps.
func NewManager() *Manager {
	return &Manager{
		AccountValues:    make(map[string]*account.AccountValue),
		Portfolio:        make(map[string]map[int64]*account.PortfolioItem),
		Positions:        make(map[string]map[int64]*account.Position),
		Trades:           make(map[OrderKey]*order.Trade),
		PermID2Trade:     make(map[int64]*order.Trade),
		Fills:            make(map[string]*order.Fill),
		Tickers:          make(map[int64]*market.Ticker),
		PendingTickers:   make(map[*market.Ticker]bool),
		ReqID2Ticker:     make(map[int64]*market.Ticker),
		ReqID2Subscriber: make(map[int64]interface{}),
		ReqID2PnL:        make(map[int64]*account.PnL),
		ReqID2PnLSingle:  make(map[int64]*account.PnLSingle),
		Requests:         NewRequestMap(),
		Results:          make(map[int64]interface{}),
		ReqID2Contract:   make(map[int64]*contract.Contract),
		NewsBulletins:    make(map[int]*account.NewsBulletin),
	}
}

// StartReq begins tracking a request-response cycle.
func (m *Manager) StartReq(reqID int64, con *contract.Contract) <-chan Result {
	if con != nil {
		m.Mu.Lock()
		m.ReqID2Contract[reqID] = con
		m.Mu.Unlock()
	}
	m.Results[reqID] = nil
	return m.Requests.Start(reqID)
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
	existing, ok := m.Results[reqID]
	if !ok {
		return
	}
	if existing == nil {
		m.Results[reqID] = []interface{}{item}
	} else {
		m.Results[reqID] = append(existing.([]interface{}), item)
	}
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

// Reset clears all state (called on disconnect).
func (m *Manager) Reset() {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	m.AccountValues = make(map[string]*account.AccountValue)
	m.Portfolio = make(map[string]map[int64]*account.PortfolioItem)
	m.Positions = make(map[string]map[int64]*account.Position)
	m.Trades = make(map[OrderKey]*order.Trade)
	m.PermID2Trade = make(map[int64]*order.Trade)
	m.Fills = make(map[string]*order.Fill)
	m.Tickers = make(map[int64]*market.Ticker)
	m.PendingTickers = make(map[*market.Ticker]bool)
	m.ReqID2Ticker = make(map[int64]*market.Ticker)
	m.ReqID2Subscriber = make(map[int64]interface{})
	m.ReqID2PnL = make(map[int64]*account.PnL)
	m.ReqID2PnLSingle = make(map[int64]*account.PnLSingle)
	m.Results = make(map[int64]interface{})
	m.ReqID2Contract = make(map[int64]*contract.Contract)
	m.NewsBulletins = make(map[int]*account.NewsBulletin)
	m.Accounts = nil
}
