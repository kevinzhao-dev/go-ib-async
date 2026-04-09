package state

import (
	"github.com/kevinzhao-dev/go-ib-async/market"
)

// MarketState holds all market data related state.
type MarketState struct {
	Tickers        map[int64]*market.Ticker
	PendingTickers map[*market.Ticker]bool
	ReqID2Ticker   map[int64]*market.Ticker
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

// ClearPendingTickers clears pending tickers and resets their tick lists.
// Called at the start of each network packet (tcpDataArrived).
func (m *MarketState) ClearPendingTickers() {
	for ticker := range m.PendingTickers {
		ticker.Ticks = nil
		ticker.TickByTicks = nil
		ticker.DomTicks = nil
	}
	m.PendingTickers = make(map[*market.Ticker]bool)
}
