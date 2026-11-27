package ibgo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// --- Open orders & executions ---

// ReqOpenOrders requests the open orders of this API client (with client id
// 0 also manual TWS orders) and returns their trades. Existing Trade objects
// are updated in place; orders not yet known are added to the state.
//
// With client id 0 TWS also binds the manual orders to the API, which can
// cancel and resubmit a working order (losing its queue priority). Use
// ReqAllOpenOrders to only read them. cmd/ibguard blocks reqOpenOrders from
// client id 0.
//
// TWS rejects reqOpenOrders when the API is in read-only mode; the error
// then matches ErrReadOnlyAPI (ReqAllOpenOrders still works).
func (ib *IB) ReqOpenOrders(ctx context.Context) ([]*order.Trade, error) {
	return ib.reqOpenOrders(ctx, true, false, ib.client.ReqOpenOrders)
}

// ReqAllOpenOrders requests the open orders of all API clients and TWS.
func (ib *IB) ReqAllOpenOrders(ctx context.Context) ([]*order.Trade, error) {
	return ib.reqOpenOrders(ctx, false, false, ib.client.ReqAllOpenOrders)
}

// OpenOrdersSnapshot returns the open orders of all API clients and TWS
// (reqAllOpenOrders) without making the client track other clients' orders.
// This client's orders, and orders it already tracks (as the Master API
// client does), are its live trades. Other orders come back as detached
// trades: they are not added to Trades or OpenTrades, fire no events and are
// not journaled, because TWS sends this client no updates for them and a
// tracked copy would go stale. Risk checks and the journal therefore see the
// same orders as before the call.
func (ib *IB) OpenOrdersSnapshot(ctx context.Context) ([]*order.Trade, error) {
	return ib.reqOpenOrders(ctx, false, true, ib.client.ReqAllOpenOrders)
}

func (ib *IB) reqOpenOrders(ctx context.Context, roRejected, detach bool, send func() error) ([]*order.Trade, error) {
	v, err := ib.runSingleDetach(ctx, ib.openOrdersReq, roRejected, detach, send)
	if err != nil {
		return nil, err
	}
	items := resultItems(v)
	seen := make(map[*order.Trade]bool, len(items))
	trades := make([]*order.Trade, 0, len(items))
	for _, item := range items {
		if t, ok := item.(*order.Trade); ok && !seen[t] {
			seen[t] = true
			trades = append(trades, t)
		}
	}
	return trades, nil
}

// ReqAutoOpenOrders (client id 0 only) makes TWS bind newly created manual
// orders to this client so they get an order id and can be modified via API.
func (ib *IB) ReqAutoOpenOrders(autoBind bool) error {
	return ib.client.ReqAutoOpenOrders(autoBind)
}

// ReqExecutions requests today's executions matching filter (the zero filter
// matches everything). New executions are linked to their trades exactly like
// live fills, but without firing the live fill events.
func (ib *IB) ReqExecutions(ctx context.Context, filter order.ExecutionFilter) ([]order.Fill, error) {
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, nil)
	err := ib.client.ReqExecutions(reqID, protocol.ExecutionFilter{
		ClientID: filter.ClientID,
		AcctCode: filter.AcctCode,
		Time:     filter.Time,
		Symbol:   filter.Symbol,
		SecType:  filter.SecType,
		Exchange: filter.Exchange,
		Side:     filter.Side,
	})
	if err != nil {
		ib.state.CancelReq(reqID)
		return nil, err
	}
	v, err := ib.awaitReq(ctx, reqID, ch)
	if err != nil {
		return nil, err
	}
	items := resultItems(v)
	fills := make([]order.Fill, 0, len(items))
	// Copy under the state lock: the reader sets CommissionReport on these
	// fills (handleCommissionReport) while holding it.
	ib.state.Mu.RLock()
	for _, item := range items {
		if f, ok := item.(*order.Fill); ok {
			fills = append(fills, *f)
		}
	}
	ib.state.Mu.RUnlock()
	return fills, nil
}

// TradeFills returns copies of the fills of t. Unlike t.FillsSnapshot, the
// copies are taken under the lock the reader holds when it attaches a
// commission report, so their CommissionReport can be read without a race.
func (ib *IB) TradeFills(t *order.Trade) []order.Fill {
	if t == nil {
		return nil
	}
	ib.state.Mu.RLock()
	defer ib.state.Mu.RUnlock()
	ptrs := t.FillsSnapshot()
	fills := make([]order.Fill, 0, len(ptrs))
	for _, f := range ptrs {
		fills = append(fills, *f)
	}
	return fills
}

// --- Order synchronisation ---

// SyncOptions controls SyncOrders.
type SyncOptions struct {
	// AllClients uses reqAllOpenOrders (orders of every API client and TWS)
	// instead of reqOpenOrders (this client's orders only). With client id
	// 0 SyncOrders never sends reqOpenOrders, which would bind manual TWS
	// orders: it filters reqAllOpenOrders to client 0 instead.
	AllClients bool
	// ExecFilter restricts the executions request (zero value: all of today's).
	ExecFilter order.ExecutionFilter
	// SkipPositions skips refreshing positions.
	SkipPositions bool
	// IncludeCompleted always fetches completed orders (normally only done
	// when locally active trades went missing). Use it after a process
	// restart so orders completed while the process was down, and their
	// fills, become known again (e.g. to reconcile with a journal).
	IncludeCompleted bool
}

// SyncReport summarises a SyncOrders pass.
type SyncReport struct {
	// OpenTrades are the orders the broker reports as open.
	OpenTrades []*order.Trade
	// NewFills is the number of executions that were not known before.
	NewFills int
	// Reconciled are locally active trades that the broker no longer lists
	// as open and whose final status was taken from completed orders.
	Reconciled []*order.Trade
	// Unresolved are locally active trades the broker reports neither as
	// open nor as completed. They need manual attention.
	Unresolved []*order.Trade
	// Positions is the refreshed position list (unless SkipPositions).
	Positions []account.Position
	// ReadOnly is set when TWS/Gateway is in read-only API mode. Open
	// orders then come from reqAllOpenOrders (filtered to this client
	// unless AllClients) and completed orders are unavailable, so stale
	// trades end up in Unresolved.
	ReadOnly bool
}

// SyncOrders reconciles local order state with the broker. It is meant to be
// run after (re)connecting: it refreshes open orders, fetches today's
// executions (adding missed fills to their trades), finalises trades that
// were completed while disconnected and refreshes positions.
//
// Trade objects already held by the application are updated in place and
// fire their usual events (status, filled, cancelled).
func (ib *IB) SyncOrders(ctx context.Context, opts SyncOptions) (*SyncReport, error) {
	report := &SyncReport{}

	myClient := ib.ClientID()
	// This client's orders out of reqAllOpenOrders.
	ownOpenOrders := func() ([]*order.Trade, error) {
		all, err := ib.ReqAllOpenOrders(ctx)
		var own []*order.Trade
		for _, t := range all {
			if t.Order.ClientID == myClient {
				own = append(own, t)
			}
		}
		return own, err
	}
	var open []*order.Trade
	var err error
	switch {
	case opts.AllClients:
		open, err = ib.ReqAllOpenOrders(ctx)
	case myClient == 0:
		// reqOpenOrders from client 0 binds manual TWS orders: read only.
		open, err = ownOpenOrders()
	default:
		open, err = ib.ReqOpenOrders(ctx)
		if errors.Is(err, ErrReadOnlyAPI) {
			report.ReadOnly = true
			open, err = ownOpenOrders()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("sync open orders: %w", err)
	}
	report.OpenTrades = open
	openSet := make(map[*order.Trade]bool, len(open))
	for _, t := range open {
		openSet[t] = true
	}

	var stale []*order.Trade
	for _, t := range ib.state.GetTrades() {
		if t.IsDone() || openSet[t] {
			continue
		}
		if !opts.AllClients && t.Order.ClientID != myClient {
			continue // reqOpenOrders does not report other clients' orders
		}
		stale = append(stale, t)
	}

	// Completed orders before executions, so that fills of orders completed
	// while we were away attach to their (completed) trades.
	var completed []*order.Trade
	if len(stale) > 0 || opts.IncludeCompleted {
		completed, err = ib.ReqCompletedOrders(ctx, false)
		if errors.Is(err, ErrReadOnlyAPI) {
			report.ReadOnly = true
			completed, err = nil, nil
		}
		if err != nil {
			return report, fmt.Errorf("sync completed orders: %w", err)
		}
	}

	before := ib.numFills()
	if _, err := ib.ReqExecutions(ctx, opts.ExecFilter); err != nil {
		return report, fmt.Errorf("sync executions: %w", err)
	}
	report.NewFills = ib.numFills() - before

	if len(stale) > 0 {
		byPerm := make(map[int64]*order.Trade, len(completed))
		for _, c := range completed {
			if p := c.Order.PermID; p != 0 {
				byPerm[p] = c
			}
		}
		for _, t := range stale {
			c := byPerm[t.PermID()]
			if c == nil || !order.DoneStates[c.Status()] {
				report.Unresolved = append(report.Unresolved, t)
				continue
			}
			filled := -1.0
			if fq := c.Order.FilledQuantity; fq != UnsetDouble {
				filled = fq
			}
			ib.finalizeTrade(t, c.Status(), filled, "Sync: completed order")
			report.Reconciled = append(report.Reconciled, t)
		}
	}

	if !opts.SkipPositions {
		pos, err := ib.ReqPositions(ctx)
		if err != nil {
			return report, fmt.Errorf("sync positions: %w", err)
		}
		report.Positions = pos
	}
	return report, nil
}

func (ib *IB) numFills() int {
	ib.state.Mu.RLock()
	defer ib.state.Mu.RUnlock()
	return len(ib.state.Fills)
}

// finalizeTrade moves a trade to a terminal status and fires the events.
func (ib *IB) finalizeTrade(t *order.Trade, status string, filled float64, msg string) {
	old := t.ApplyFinalStatus(status, filled, time.Now(), msg)
	ib.journalStatus(t)
	ib.OrderStatusEvent.Emit(t)
	t.StatusEvent.Emit(t)
	if status != old {
		switch status {
		case order.StatusFilled:
			t.FilledEvent.Emit(t)
		case order.StatusCancelled, order.StatusApiCancelled:
			t.CancelledEvent.Emit(t)
		}
	}
}

// --- What-if ---

// WhatIfOrder asks TWS for the margin and commission impact of an order
// without placing it. The given order is not modified, and neither the risk
// manager nor the journal is involved since nothing can execute.
func (ib *IB) WhatIfOrder(ctx context.Context, con *contract.Contract, ord *order.Order) (*order.OrderState, error) {
	if con == nil || ord == nil {
		return nil, errors.New("ibgo: WhatIfOrder needs a contract and an order")
	}
	if !ib.client.IsConnected() {
		return nil, ErrNotConnected
	}
	wi := *ord // shallow copy: the caller's order stays untouched
	reqID := ib.client.GetReqID()
	wi.OrderID = reqID
	wi.WhatIf = true
	wi.Transmit = true
	if !strings.HasPrefix(wi.OrderType, "VOL") {
		wi.Volatility = UnsetDouble
	}

	ch := ib.state.StartReq(reqID, con)
	if err := ib.sendPlaceOrder(reqID, con, &wi); err != nil {
		ib.state.CancelReq(reqID)
		return nil, err
	}
	v, err := ib.awaitReq(ctx, reqID, ch)
	if err != nil {
		return nil, err
	}
	st, ok := v.(*order.OrderState)
	if !ok || st == nil {
		return nil, errors.New("ibgo: unexpected what-if response")
	}
	return st, nil
}

// --- Bracket orders ---

// BracketOrder creates a limit entry bracketed by a take-profit limit and a
// stop-loss stop, with order ids assigned, children linked to the parent and
// transmit flags set so the last leg releases the group. Submit it with
// PlaceBracketOrder. Prices are validated for the side (see order.NewBracket).
func (ib *IB) BracketOrder(action string, quantity, limitPrice, takeProfitPrice, stopLossPrice float64) (*order.BracketOrder, error) {
	b, err := order.NewBracket(action, quantity, limitPrice, takeProfitPrice, stopLossPrice)
	if err != nil {
		return nil, err
	}
	b.Parent.OrderID = ib.client.GetReqID()
	b.TakeProfit.OrderID = ib.client.GetReqID()
	b.StopLoss.OrderID = ib.client.GetReqID()
	b.Link()
	return b, nil
}

// PlaceBracketOrder submits the three legs of a bracket in order. All legs
// are risk-checked before the first is sent. If a later leg cannot be sent,
// the legs already sent (still untransmitted, since only the last leg has
// Transmit=true) are cancelled and an error is returned together with the
// trades that were created.
func (ib *IB) PlaceBracketOrder(con *contract.Contract, b *order.BracketOrder) ([]*order.Trade, error) {
	if con == nil || b == nil || b.Parent == nil || b.TakeProfit == nil || b.StopLoss == nil {
		return nil, errors.New("ibgo: PlaceBracketOrder needs a contract and all three legs")
	}
	legs := b.Orders()
	for _, leg := range legs {
		if leg.WhatIf {
			return nil, ErrUseWhatIfOrder
		}
	}

	ib.placeMu.Lock()
	defer ib.placeMu.Unlock()

	if !ib.client.IsConnected() {
		return nil, ErrNotConnected
	}
	for _, leg := range legs {
		if leg.OrderID == 0 {
			leg.OrderID = ib.client.GetReqID()
		}
	}
	b.Link()

	names := []string{"parent", "take-profit", "stop-loss"}
	if rc := ib.riskChecker(); rc != nil {
		bc, _ := rc.(BracketChecker)
		for i, leg := range legs {
			var err error
			if bc != nil {
				err = bc.CheckBracketLeg(con, leg, i)
			} else {
				err = rc.CheckOrder(con, leg, false)
			}
			if err != nil {
				return nil, fmt.Errorf("ibgo: bracket %s leg rejected: %w", names[i], err)
			}
		}
	}

	trades := make([]*order.Trade, 0, len(legs))
	for i, leg := range legs {
		t, err := ib.placeOrderLocked(con, leg, false)
		if err != nil {
			for _, sent := range trades {
				_, _ = ib.CancelOrder(sent.Order)
			}
			return trades, fmt.Errorf("ibgo: bracket %s leg failed: %w", names[i], err)
		}
		trades = append(trades, t)
	}
	return trades, nil
}
