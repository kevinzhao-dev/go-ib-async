package state

import (
	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

// OrderState holds all order/trade/fill related state.
type OrderState struct {
	Trades       map[OrderKey]*order.Trade
	PermID2Trade map[int64]*order.Trade
	Fills        map[string]*order.Fill // execID → fill
}

func newOrderState() OrderState {
	return OrderState{
		Trades:       make(map[OrderKey]*order.Trade),
		PermID2Trade: make(map[int64]*order.Trade),
		Fills:        make(map[string]*order.Fill),
	}
}

func (o *OrderState) reset() {
	o.Trades = make(map[OrderKey]*order.Trade)
	o.PermID2Trade = make(map[int64]*order.Trade)
	o.Fills = make(map[string]*order.Fill)
}

// AccountState holds all account/portfolio/position state.
type AccountState struct {
	AccountValues map[string]*account.AccountValue
	Portfolio     map[string]map[int64]*account.PortfolioItem
	Positions     map[string]map[int64]*account.Position
	PnLs          map[int64]*account.PnL
	PnLSingles    map[int64]*account.PnLSingle

	// Positions retained after disconnect, awaiting confirmation on the
	// next connection. Account and conId together identify a position.
	unconfirmedPositions map[positionKey]bool
}

type positionKey struct {
	account string
	conID   int64
}

func newAccountState() AccountState {
	return AccountState{
		AccountValues: make(map[string]*account.AccountValue),
		Portfolio:     make(map[string]map[int64]*account.PortfolioItem),
		Positions:     make(map[string]map[int64]*account.Position),
		PnLs:          make(map[int64]*account.PnL),
		PnLSingles:    make(map[int64]*account.PnLSingle),
	}
}

func (a *AccountState) reset() {
	a.AccountValues = make(map[string]*account.AccountValue)
	a.Portfolio = make(map[string]map[int64]*account.PortfolioItem)
	a.Positions = make(map[string]map[int64]*account.Position)
	a.PnLs = make(map[int64]*account.PnL)
	a.PnLSingles = make(map[int64]*account.PnLSingle)
	a.unconfirmedPositions = nil
}

// MarkPositionsUnconfirmed retains the last view until the next full download.
func (m *Manager) MarkPositionsUnconfirmed() {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.unconfirmedPositions = make(map[positionKey]bool)
	for acct, rows := range m.Positions {
		for conID := range rows {
			m.unconfirmedPositions[positionKey{acct, conID}] = true
		}
	}
}

// UpdatePosition applies a broker row, including live updates after positionEnd.
func (m *Manager) UpdatePosition(p *account.Position) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	conID := p.Contract.ConID
	delete(m.unconfirmedPositions, positionKey{p.Account, conID})
	rows := m.Positions[p.Account]
	if p.Position == 0 {
		delete(rows, conID)
		return
	}
	if rows == nil {
		rows = make(map[int64]*account.Position)
		m.Positions[p.Account] = rows
	}
	rows[conID] = p
}

// FinishPositions removes positions closed while disconnected. Return zero
// rows so event subscribers see the same changes as snapshot readers.
func (m *Manager) FinishPositions() []account.Position {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	var closed []account.Position
	for k := range m.unconfirmedPositions {
		rows := m.Positions[k.account]
		p := *rows[k.conID]
		p.Position, p.AvgCost = 0, 0
		closed = append(closed, p)
		delete(rows, k.conID)
	}
	m.unconfirmedPositions = nil
	return closed
}
