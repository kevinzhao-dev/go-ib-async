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
}
