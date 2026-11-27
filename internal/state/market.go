package state

import (
	"github.com/kevinzhao-dev/go-ib-async/market"
)

// MarketState holds all market data related state.
type MarketState struct {
	Tickers          map[int64]*market.Ticker
	PendingTickers   map[*market.Ticker]bool
	ReqID2Ticker     map[int64]*market.Ticker
	ReqID2Subscriber map[int64]interface{}
}

func newMarketState() MarketState {
	return MarketState{
		Tickers:          make(map[int64]*market.Ticker),
		PendingTickers:   make(map[*market.Ticker]bool),
		ReqID2Ticker:     make(map[int64]*market.Ticker),
		ReqID2Subscriber: make(map[int64]interface{}),
	}
}

func (m *MarketState) reset() {
	m.Tickers = make(map[int64]*market.Ticker)
	m.PendingTickers = make(map[*market.Ticker]bool)
	m.ReqID2Ticker = make(map[int64]*market.Ticker)
	m.ReqID2Subscriber = make(map[int64]interface{})
}

// clearTickLists resets the per-batch tick lists of the tickers. It takes
// each ticker's lock, so it must not be called while holding Manager.Mu:
// tick handlers take Manager.Mu while holding a ticker lock, and users may
// call into the client while holding a ticker's read lock.
func clearTickLists(tickers map[*market.Ticker]bool) {
	for ticker := range tickers {
		ticker.Lock()
		ticker.Ticks = nil
		ticker.TickByTicks = nil
		ticker.DomTicks = nil
		ticker.Unlock()
	}
}
