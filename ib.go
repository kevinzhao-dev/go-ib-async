// Package ibgo provides a Go client for the Interactive Brokers TWS/Gateway API.
package ibgo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/event"
	"github.com/kevinzhao-dev/go-ib-async/internal/state"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// IB is the main client for interacting with Interactive Brokers.
type IB struct {
	client *protocol.Client
	state  *state.Manager

	// Synthetic request key generator (always negative, unique)
	syntheticReqID atomic.Int64

	// Requests whose responses carry no request id (IB sends one shared
	// response stream): only one of each may be in flight.
	positionsReq  *singleReq
	completedReq  *singleReq
	openOrdersReq *singleReq

	// placeMu serialises pre-trade risk checks, journal write-ahead and order
	// registration so two goroutines cannot both pass a limit check that only
	// one of them should.
	placeMu sync.Mutex

	// serverTime is the last time reported by reqCurrentTime (unix seconds).
	serverTime atomic.Int64

	// snapshots defers tickSnapshotEnd to the end of the network batch.
	snapshots snapshotState

	hooksMu sync.RWMutex
	risk    PreTradeChecker
	journal OrderJournal

	// Streaming subscriptions by request id (see subscriptions.go).
	subsMu sync.Mutex
	subs   map[int64]*subscription
	// subOpMu serialises cancelling a subscription with re-sending it
	// after a reconnect.
	subOpMu sync.Mutex
	// connEpoch counts successful connections; a subscription is live on
	// the connection whose epoch it was sent on.
	connEpoch atomic.Int64
	// mdType is the last market data type requested (0 = never).
	mdType atomic.Int64
	// depthFresh holds depth requests whose book is emptied before their
	// first update (see depthStarted).
	depthMu    sync.Mutex
	depthFresh map[int64]bool

	// acctMu guards the account updates subscription (nil = none) and
	// serialises the requests that change it (subscribe, cancel, restore)
	// so they reach TWS in the order their effect is recorded.
	acctMu      sync.Mutex
	acctUpdates *string
	acctReq     *singleReq
	timeReq     *singleReq
	// acctDL is the running account download (see beginAcctDownload).
	dlMu   sync.Mutex
	acctDL *acctDownload

	pnlMu     sync.Mutex
	pnl       map[pnlKey]*account.PnL
	pnlSingle map[pnlKey]*account.PnLSingle

	ruleMu      sync.Mutex
	ruleWaiters map[int][]int64

	// Events
	ConnectedEvent    event.Event[struct{}]
	DisconnectedEvent event.Event[struct{}]
	ErrorEvent        event.Event[*IBError]
	UpdateEvent       event.Event[struct{}]

	// Order events
	NewOrderEvent         event.Event[*order.Trade]
	OrderModifyEvent      event.Event[*order.Trade]
	CancelOrderEvent      event.Event[*order.Trade]
	OpenOrderEvent        event.Event[*order.Trade]
	OrderStatusEvent      event.Event[*order.Trade]
	ExecDetailsEvent      event.Event[*order.Fill]
	CommissionReportEvent event.Event[*order.Fill]

	// Market data events
	PendingTickersEvent event.Event[[]*market.Ticker]

	// Account events
	PositionEvent     event.Event[*account.Position]
	AccountValueEvent event.Event[*account.AccountValue]
	PortfolioEvent    event.Event[*account.PortfolioItem]
	PnLEvent          event.Event[*account.PnL]
	PnLSingleEvent    event.Event[*account.PnLSingle]
}

// IBError wraps an IB API error with request context.
type IBError struct {
	ReqID    int64
	Code     int
	Message  string
	Contract *contract.Contract
	// AdvancedOrderReject holds the JSON reject reason sent by newer servers.
	AdvancedOrderReject string
	// IsWarning is true for codes TWS uses as warnings (the request or order
	// is still alive).
	IsWarning bool
}

func (e *IBError) Error() string {
	return fmt.Sprintf("ibgo: error %d (reqId %d): %s", e.Code, e.ReqID, e.Message)
}

// New creates a new IB client.
func New() *IB {
	ib := &IB{
		client:        protocol.NewClient(),
		state:         state.NewManager(),
		positionsReq:  newSingleReq(),
		completedReq:  newSingleReq(),
		openOrdersReq: newSingleReq(),
		subs:          map[int64]*subscription{},
		depthFresh:    map[int64]bool{},
		acctReq:       newSingleReq(),
		timeReq:       newSingleReq(),
		pnl:           map[pnlKey]*account.PnL{},
		pnlSingle:     map[pnlKey]*account.PnLSingle{},
		ruleWaiters:   map[int][]int64{},
	}
	ib.client.OnMessage = ib.handleMessage
	ib.client.OnDisconnect = func(err error) {
		ib.state.Requests.DrainAll(ErrDisconnected)
		ib.DisconnectedEvent.Emit(struct{}{})
	}
	ib.client.OnDataArrived = func() {
		// Set timestamp for this message batch. Pending tickers accumulate
		// across all messages of the batch and are cleared in OnDataProcessed.
		ib.state.SetLastTime(time.Now())
	}
	ib.client.OnDataProcessed = func() {
		ib.UpdateEvent.Emit(struct{}{})
		pending := ib.state.GetPendingTickers()
		if len(pending) > 0 {
			t, ts := ib.state.GetLastTime()
			for _, ticker := range pending {
				ticker.Lock()
				ticker.Time = t
				ticker.Timestamp = ts
				ticker.Unlock()
				ticker.UpdateEvent.Emit(ticker)
			}
			ib.PendingTickersEvent.Emit(pending)
		}
		// Clear pending tickers and their tick lists AFTER emitting events
		ib.state.ClearPendingTickers()
		// Snapshot requests complete only now that their tickers are final.
		ib.flushSnapshotEnds()
	}
	return ib
}

// Connect establishes a connection to TWS/Gateway.
func (ib *IB) Connect(ctx context.Context, host string, port, clientID int) error {
	err := ib.client.Connect(ctx, host, port, clientID)
	if err != nil {
		return err
	}
	// Before ConnectedEvent: subscriptions made from its handlers belong to
	// this connection and are not re-sent by restoreSubscriptions, and they
	// get the market data type chosen before the reconnect.
	ib.connEpoch.Add(1)
	if t := ib.mdType.Load(); t != 0 {
		if err := ib.client.ReqMarketDataType(int(t)); err != nil {
			log.Printf("ibgo: restore market data type: %v", err)
		}
	}
	ib.ConnectedEvent.Emit(struct{}{})
	return nil
}

// Disconnect closes the connection and clears all client-side state.
func (ib *IB) Disconnect() {
	ib.client.Disconnect()
	ib.state.Reset()
	ib.clearSubs()
	ib.mdType.Store(0)
}

// IsConnected returns true if connected to TWS/Gateway.
func (ib *IB) IsConnected() bool {
	return ib.client.IsConnected()
}

// ServerVersion returns the TWS/Gateway server version.
func (ib *IB) ServerVersion() int {
	return ib.client.ServerVersion()
}

// ManagedAccounts returns the list of managed account IDs.
func (ib *IB) ManagedAccounts() []string {
	return ib.client.Accounts()
}

// ClientID returns the API client id of the current/last connection.
func (ib *IB) ClientID() int64 {
	return int64(ib.client.CurrentClientID())
}

// now returns the timestamp to use for trade log entries.
func (ib *IB) now() time.Time {
	if t, _ := ib.state.GetLastTime(); !t.IsZero() {
		return t
	}
	return time.Now()
}

// awaitReq waits for a tracked request to finish.
func (ib *IB) awaitReq(ctx context.Context, reqID int64, ch <-chan state.Result) (interface{}, error) {
	done := ib.client.Done()
	select {
	case result, ok := <-ch:
		if !ok {
			return nil, ErrRequestCancelled
		}
		return result.Value, result.Err
	case <-ctx.Done():
		ib.state.CancelReq(reqID)
		return nil, ctx.Err()
	case <-done:
		ib.state.CancelReq(reqID)
		return nil, ErrDisconnected
	}
}

// resultItems converts an accumulated multi-message result to a slice.
func resultItems(v interface{}) []interface{} {
	if items, ok := v.([]interface{}); ok {
		return items
	}
	return nil
}

// --- Request-Response Methods ---

// ReqContractDetails requests full contract details.
func (ib *IB) ReqContractDetails(ctx context.Context, con *contract.Contract) ([]contract.ContractDetails, error) {
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, con)

	if err := ib.client.ReqContractDetails(reqID, con); err != nil {
		ib.state.CancelReq(reqID)
		return nil, err
	}

	v, err := ib.awaitReq(ctx, reqID, ch)
	if err != nil {
		return nil, err
	}
	items := resultItems(v)
	details := make([]contract.ContractDetails, 0, len(items))
	for _, item := range items {
		if cd, ok := item.(*contract.ContractDetails); ok {
			details = append(details, *cd)
		}
	}
	return details, nil
}

// ReqPositions requests all positions.
// Serialized: IB sends one positionEnd for all outstanding position requests,
// so concurrent callers wait for each other.
func (ib *IB) ReqPositions(ctx context.Context) ([]account.Position, error) {
	if _, err := ib.runSingle(ctx, ib.positionsReq, false, ib.client.ReqPositions); err != nil {
		return nil, err
	}
	return ib.state.GetPositions(), nil
}

// ReqAccountSummary requests account summary values.
func (ib *IB) ReqAccountSummary(ctx context.Context, groupName, tags string) ([]account.AccountValue, error) {
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, nil)

	if err := ib.client.ReqAccountSummary(reqID, groupName, tags); err != nil {
		ib.state.CancelReq(reqID)
		return nil, err
	}

	v, err := ib.awaitReq(ctx, reqID, ch)
	// The summary is a subscription in TWS: always cancel it so repeated
	// calls do not hit the "max 2 account summary requests" limit.
	_ = ib.client.CancelAccountSummary(reqID)
	if err != nil {
		return nil, err
	}
	items := resultItems(v)
	values := make([]account.AccountValue, 0, len(items))
	for _, item := range items {
		if av, ok := item.(*account.AccountValue); ok {
			values = append(values, *av)
		}
	}
	return values, nil
}

// --- State Accessors ---

// Positions returns a snapshot of all current positions.
func (ib *IB) Positions() []account.Position {
	return ib.state.GetPositions()
}

// Trades returns all trades.
func (ib *IB) Trades() []*order.Trade {
	return ib.state.GetTrades()
}

// OpenTrades returns active trades.
func (ib *IB) OpenTrades() []*order.Trade {
	return ib.state.GetOpenTrades()
}

// Fills returns all fills.
func (ib *IB) Fills() []order.Fill {
	return ib.state.GetFills()
}

// AccountValues returns all account values.
func (ib *IB) AccountValues() []account.AccountValue {
	return ib.state.GetAccountValues()
}

// --- Message Handler ---

func (ib *IB) handleMessage(msgID int, r *protocol.FieldReader) {
	switch msgID {
	case protocol.InMsgErrMsg:
		ib.handleError(r)
	case protocol.InMsgContractDetails:
		ib.handleContractDetails(r)
	case protocol.InMsgContractDetailsEnd:
		ib.handleContractDetailsEnd(r)
	case protocol.InMsgPosition:
		ib.handlePosition(r)
	case protocol.InMsgPositionEnd:
		ib.handlePositionEnd(r)
	case protocol.InMsgUpdateAccountValue:
		ib.handleUpdateAccountValue(r)
	case protocol.InMsgAccountDownloadEnd:
		ib.handleAccountDownloadEnd(r)
	case protocol.InMsgAccountSummary:
		ib.handleAccountSummary(r)
	case protocol.InMsgAccountSummaryEnd:
		ib.handleAccountSummaryEnd(r)
	case protocol.InMsgOrderStatus:
		ib.handleOrderStatus(r)
	case protocol.InMsgOpenOrder:
		ib.handleOpenOrder(r)
	case protocol.InMsgOpenOrderEnd:
		ib.handleOpenOrderEnd(r)
	case protocol.InMsgExecDetails:
		ib.handleExecDetails(r)
	case protocol.InMsgExecDetailsEnd:
		ib.handleExecDetailsEnd(r)
	case protocol.InMsgCommissionReport:
		ib.handleCommissionReport(r)
	case protocol.InMsgUpdatePortfolio:
		ib.handleUpdatePortfolio(r)
	case protocol.InMsgRealtimeBar:
		ib.handleRealtimeBar(r)
	case protocol.InMsgSecDefOptParams:
		ib.handleSecDefOptParams(r)
	case protocol.InMsgSecDefOptParamsEnd:
		ib.handleSecDefOptParamsEnd(r)
	case protocol.InMsgCompletedOrder:
		ib.handleCompletedOrder(r)
	case protocol.InMsgCompletedOrdersEnd:
		ib.handleCompletedOrdersEnd(r)
	case protocol.InMsgOrderBound:
		ib.handleOrderBound(r)
	case protocol.InMsgCurrentTime:
		ib.handleCurrentTime(r)
	case protocol.InMsgNextValidID:
		// already handled in client.go readLoop
	case protocol.InMsgManagedAccounts:
		// already handled in client.go readLoop
	case protocol.InMsgTickPrice:
		ib.handleTickPrice(r)
	case protocol.InMsgTickSize:
		ib.handleTickSize(r)
	case protocol.InMsgTickGeneric:
		ib.handleTickGeneric(r)
	case protocol.InMsgTickString:
		ib.handleTickString(r)
	case protocol.InMsgHistoricalData:
		ib.handleHistoricalData(r)
	case protocol.InMsgHistoricalDataUpdate:
		ib.handleHistoricalDataUpdate(r)
	case protocol.InMsgTickSnapshotEnd:
		ib.handleTickSnapshotEnd(r)
	case protocol.InMsgMarketDataType:
		ib.handleMarketDataType(r)
	case protocol.InMsgTickOptionComp:
		ib.handleTickOptionComputation(r)
	case protocol.InMsgTickEFP:
		ib.handleTickEFP(r)
	case protocol.InMsgTickReqParams:
		ib.handleTickReqParams(r)
	case protocol.InMsgTickByTick:
		ib.handleTickByTick(r)
	case protocol.InMsgUpdateMktDepth:
		ib.handleMktDepth(r)
	case protocol.InMsgUpdateMktDepthL2:
		ib.handleMktDepthL2(r)
	case protocol.InMsgHeadTimestamp:
		ib.handleHeadTimestamp(r)
	case protocol.InMsgHistoricalTicks:
		ib.handleHistoricalTicks(r)
	case protocol.InMsgHistoricalTicksBidAsk:
		ib.handleHistoricalTicksBidAsk(r)
	case protocol.InMsgHistoricalTicksLast:
		ib.handleHistoricalTicksLast(r)
	case protocol.InMsgPnL:
		ib.handlePnL(r)
	case protocol.InMsgPnLSingle:
		ib.handlePnLSingle(r)
	case protocol.InMsgSymbolSamples:
		ib.handleSymbolSamples(r)
	case protocol.InMsgMarketRule:
		ib.handleMarketRule(r)
	case protocol.InMsgUpdateAccountTime:
		// account update timestamp: not tracked
	default:
		// Unhandled message types are silently ignored for now
	}
}

// warningCodes are error codes TWS uses for warnings: the request or the
// order is still alive (mirrors ib_async).
var warningCodes = map[int]bool{
	105: true, 110: true, 165: true, 317: true, 321: true, 329: true,
	399: true, 404: true, 434: true, 492: true, 10167: true,
	// "Part of requested market data is not subscribed / requires an
	// additional subscription": the rest of the data keeps streaming.
	10090: true, 10091: true,
}

func isWarningCode(code int) bool {
	return warningCodes[code] || (code >= 2100 && code < 2200)
}

func (ib *IB) handleError(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	code := int(r.ReadInt())
	msg := r.ReadString()
	advanced := ""
	if ib.client.ServerVersion() >= 166 {
		advanced = r.ReadString()
	}

	// 2101: another API client is subscribed to a different account, so
	// the pending ReqAccountUpdates was refused.
	if code == 2101 {
		if key := ib.acctReq.active(); key != 0 {
			ibErr := &IBError{ReqID: reqID, Code: code, Message: msg, AdvancedOrderReject: advanced}
			ib.ErrorEvent.Emit(ibErr)
			ib.state.EndReq(key, nil, ibErr)
			return
		}
	}
	// Info codes (2100-2199: data farm status etc.) are logged only.
	if code >= 2100 && code < 2200 {
		log.Printf("ibgo: info %d: %s", code, msg)
		return
	}
	// 317: TWS reset the market depth book; it re-sends it from scratch.
	if code == 317 {
		ib.resetDepth(reqID)
	}

	isRequest := reqID >= 0 && ib.state.Requests.Has(reqID)

	// reqId is a local orderId, or -1 for errors not tied to a request/order.
	var trade *order.Trade
	ib.state.Mu.RLock()
	if reqID > 0 {
		trade = ib.state.Trades[state.OrderKey{ClientID: ib.ClientID(), OrderID: reqID}]
	}
	con := ib.state.ReqID2Contract[reqID]
	ib.state.Mu.RUnlock()

	isWarning := isWarningCode(code)
	if code == 110 && isRequest {
		isWarning = false // whatIf request failed
	}
	if code == 110 && trade != nil && trade.Status() == order.StatusPendingSubmit {
		isWarning = false // invalid price for a new order must cancel it
	}
	// 321 "Error validating request": a pending request or a new order was
	// rejected outright and nothing more will arrive for it (ib_async treats
	// it as a warning, which leaves such requests hanging forever).
	if code == 321 && (isRequest || (trade != nil && trade.Status() == order.StatusPendingSubmit)) {
		isWarning = false
	}
	// Requests answered without a request id (reqOpenOrders,
	// reqCompletedOrders) are rejected with reqId -1 in read-only mode.
	var roFailed []int64
	if reqID < 0 && code == 321 {
		probe := &IBError{Code: code, Message: msg}
		if probe.IsReadOnly() {
			roFailed = ib.readOnlyRejectedSingles()
			if len(roFailed) > 0 {
				isWarning = false
			}
		}
	}

	ibErr := &IBError{
		ReqID:               reqID,
		Code:                code,
		Message:             msg,
		Contract:            con,
		AdvancedOrderReject: advanced,
		IsWarning:           isWarning,
	}
	ib.ErrorEvent.Emit(ibErr)

	now := ib.now()
	logMsg := fmt.Sprintf("Error %d: %s", code, msg)
	if isWarning {
		logMsg = fmt.Sprintf("Warning %d: %s", code, msg)
	}

	for _, key := range roFailed {
		ib.state.EndReq(key, nil, ibErr)
	}

	switch {
	case isWarning:
		// The order is still live at the broker: record the warning but
		// do not cancel the trade.
		if trade != nil && !trade.IsDone() {
			trade.SetStatus(order.StatusValidationError, now, logMsg, code)
			ib.journalError(trade, code, msg)
			ib.OrderStatusEvent.Emit(trade)
			trade.StatusEvent.Emit(trade)
		}
	case isRequest:
		ib.state.EndReq(reqID, nil, ibErr)
		ib.releaseFailedSnapshot(reqID)
	case trade != nil:
		if advanced != "" {
			trade.SetAdvancedError(advanced)
		}
		// A real error on an order means it was rejected or server-cancelled.
		if !trade.IsDone() {
			trade.SetStatus(order.StatusCancelled, now, logMsg, code)
			ib.journalError(trade, code, msg)
			ib.journalStatus(trade)
			ib.OrderStatusEvent.Emit(trade)
			trade.StatusEvent.Emit(trade)
			trade.CancelledEvent.Emit(trade)
		}
	}
}

func (ib *IB) handleContractDetails(r *protocol.FieldReader) {
	if ib.client.ServerVersion() < 164 {
		r.Skip(1) // skip version field (only for old servers)
	}
	reqID := r.ReadInt()

	cd := contract.NewContractDetails()
	c := &contract.Contract{}

	c.Symbol = r.ReadString()
	c.SecType = r.ReadString()
	lastTimes := r.ReadString()
	c.Strike = r.ReadFloat()
	c.Right = r.ReadString()
	c.Exchange = r.ReadString()
	c.Currency = r.ReadString()
	c.LocalSymbol = r.ReadString()
	cd.MarketName = r.ReadString()
	c.TradingClass = r.ReadString()
	c.ConID = r.ReadInt()
	cd.MinTick = r.ReadFloat()

	if ib.client.ServerVersion() < 164 {
		r.Skip(1) // obsolete mdSizeMultiplier
	}

	c.Multiplier = r.ReadString()
	cd.OrderTypes = r.ReadString()
	cd.ValidExchanges = r.ReadString()
	cd.PriceMagnifier = int(r.ReadInt())
	cd.UnderConID = r.ReadInt()
	cd.LongName = r.ReadString()
	c.PrimaryExchange = r.ReadString()
	cd.ContractMonth = r.ReadString()
	cd.Industry = r.ReadString()
	cd.Category = r.ReadString()
	cd.Subcategory = r.ReadString()
	cd.TimeZoneID = r.ReadString()
	cd.TradingHours = r.ReadString()
	cd.LiquidHours = r.ReadString()
	cd.EvRule = r.ReadString()
	cd.EvMultiplier = int(r.ReadInt())

	numSecIds := int(r.ReadInt())
	if numSecIds > 0 {
		cd.SecIDList = make([]contract.TagValue, numSecIds)
		for i := range numSecIds {
			cd.SecIDList[i] = contract.TagValue{
				Tag:   r.ReadString(),
				Value: r.ReadString(),
			}
		}
	}

	cd.AggGroup = int(r.ReadInt())
	cd.UnderSymbol = r.ReadString()
	cd.UnderSecType = r.ReadString()
	cd.MarketRuleIDs = r.ReadString()
	cd.RealExpirationDate = r.ReadString()
	cd.StockType = r.ReadString()

	if ib.client.ServerVersion() >= 164 {
		cd.MinSize = r.ReadFloat()
		cd.SizeIncrement = r.ReadFloat()
		cd.SuggestedSizeIncrement = r.ReadFloat()
	}

	// Parse lastTradeDateOrContractMonth from combined field
	if lastTimes != "" {
		var parts []string
		if strings.Contains(lastTimes, "-") {
			parts = strings.Split(lastTimes, "-")
		} else {
			parts = strings.Fields(lastTimes)
		}
		if len(parts) > 0 {
			c.LastTradeDateOrContractMonth = parts[0]
		}
		if len(parts) > 1 {
			cd.LastTradeTime = parts[1]
		}
		if len(parts) > 2 {
			cd.TimeZoneID = parts[2]
		}
	}

	cd.Contract = c
	ib.state.AppendResult(reqID, cd)
}

func (ib *IB) handleContractDetailsEnd(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	ib.state.EndReq(reqID, nil, nil)
}

func (ib *IB) handlePosition(r *protocol.FieldReader) {
	r.Skip(1) // version
	acct := r.ReadString()
	c := &contract.Contract{
		ConID:                        r.ReadInt(),
		Symbol:                       r.ReadString(),
		SecType:                      r.ReadString(),
		LastTradeDateOrContractMonth: r.ReadString(),
		Strike:                       r.ReadFloat(),
		Right:                        r.ReadString(),
		Multiplier:                   r.ReadString(),
		Exchange:                     r.ReadString(),
		Currency:                     r.ReadString(),
		LocalSymbol:                  r.ReadString(),
		TradingClass:                 r.ReadString(),
	}
	pos := r.ReadFloat()
	avgCost := r.ReadFloat()

	p := &account.Position{
		Account:  acct,
		Contract: c,
		Position: pos,
		AvgCost:  avgCost,
	}

	ib.state.Mu.Lock()
	if _, ok := ib.state.Positions[acct]; !ok {
		ib.state.Positions[acct] = make(map[int64]*account.Position)
	}
	if pos == 0 {
		delete(ib.state.Positions[acct], c.ConID)
	} else {
		ib.state.Positions[acct][c.ConID] = p
	}
	ib.state.Mu.Unlock()

	ib.PositionEvent.Emit(p)
}

func (ib *IB) handlePositionEnd(r *protocol.FieldReader) {
	if key := ib.positionsReq.active(); key != 0 {
		ib.state.EndReq(key, nil, nil)
	}
}

func (ib *IB) handleUpdateAccountValue(r *protocol.FieldReader) {
	r.Skip(1) // version
	tag := r.ReadString()
	value := r.ReadString()
	currency := r.ReadString()
	acct := r.ReadString()

	av := &account.AccountValue{
		Account:  acct,
		Tag:      tag,
		Value:    value,
		Currency: currency,
	}

	key := acct + ":" + tag + ":" + currency
	ib.state.Mu.Lock()
	ib.state.AccountValues[key] = av
	ib.state.Mu.Unlock()

	ib.AccountValueEvent.Emit(av)
}

func (ib *IB) handleAccountSummary(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	acct := r.ReadString()
	tag := r.ReadString()
	value := r.ReadString()
	currency := r.ReadString()

	av := &account.AccountValue{
		Account:  acct,
		Tag:      tag,
		Value:    value,
		Currency: currency,
	}
	ib.state.AppendResult(reqID, av)
}

func (ib *IB) handleAccountSummaryEnd(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	ib.state.EndReq(reqID, nil, nil)
}

func (ib *IB) handleOrderStatus(r *protocol.FieldReader) {
	orderID := r.ReadInt()
	status := r.ReadString()
	filled := r.ReadFloat()
	remaining := r.ReadFloat()
	avgFillPrice := r.ReadFloat()
	permID := r.ReadInt()
	parentID := r.ReadInt()
	lastFillPrice := r.ReadFloat()
	clientID := r.ReadInt()
	whyHeld := r.ReadString()
	mktCapPrice := r.ReadFloat()

	// Use same key logic as openOrder: try (clientId, orderId), fallback to permId for manual orders
	ib.state.Mu.RLock()
	key := orderKeyFromIDs(clientID, orderID, permID)
	trade := ib.state.Trades[key]
	if trade == nil && permID != 0 {
		// Try permId lookup for manual/cross-client orders
		trade = ib.state.PermID2Trade[permID]
	}
	ib.state.Mu.RUnlock()

	if trade == nil {
		return
	}

	oldStatus, changed := trade.UpdateOrderStatus(order.OrderStatus{
		OrderID:       orderID,
		Status:        status,
		Filled:        filled,
		Remaining:     remaining,
		AvgFillPrice:  avgFillPrice,
		PermID:        permID,
		ParentID:      parentID,
		LastFillPrice: lastFillPrice,
		ClientID:      clientID,
		WhyHeld:       whyHeld,
		MktCapPrice:   mktCapPrice,
	}, ib.now())
	if !changed {
		return // duplicate status message: no events
	}
	if permID != 0 && trade.PermID() == 0 {
		trade.SetPermID(permID)
		ib.state.Mu.Lock()
		ib.state.PermID2Trade[permID] = trade
		ib.state.Mu.Unlock()
	}

	ib.journalStatus(trade)
	ib.OrderStatusEvent.Emit(trade)
	trade.StatusEvent.Emit(trade)

	if status != oldStatus {
		if status == order.StatusFilled {
			trade.FilledEvent.Emit(trade)
		} else if status == order.StatusCancelled {
			trade.CancelledEvent.Emit(trade)
		}
	}
}

func orderKeyFromIDs(clientID, orderID, permID int64) state.OrderKey {
	if orderID <= 0 {
		return state.OrderKey{ClientID: 0, OrderID: permID}
	}
	return state.OrderKey{ClientID: clientID, OrderID: orderID}
}

func (ib *IB) handleOpenOrder(r *protocol.FieldReader) {
	o := order.NewOrder()
	c := &contract.Contract{}
	st := order.NewOrderState()

	// msgId already skipped by Decoder
	o.OrderID = r.ReadInt()

	// Contract fields
	c.ConID = r.ReadInt()
	c.Symbol = r.ReadString()
	c.SecType = r.ReadString()
	c.LastTradeDateOrContractMonth = r.ReadString()
	c.Strike = r.ReadFloat()
	c.Right = r.ReadString()
	c.Multiplier = r.ReadString()
	c.Exchange = r.ReadString()
	c.Currency = r.ReadString()
	c.LocalSymbol = r.ReadString()
	c.TradingClass = r.ReadString()

	// Order fields (first block)
	o.Action = r.ReadString()
	o.TotalQuantity = r.ReadFloat()
	o.OrderType = r.ReadString()
	o.LmtPrice = r.ReadFloat()
	o.AuxPrice = r.ReadFloat()
	o.TIF = r.ReadString()
	o.OcaGroup = r.ReadString()
	o.Account = r.ReadString()
	o.OpenClose = r.ReadString()
	o.Origin = int(r.ReadInt())
	o.OrderRef = r.ReadString()
	o.ClientID = r.ReadInt()
	o.PermID = r.ReadInt()
	o.OutsideRTH = r.ReadBool()
	o.Hidden = r.ReadBool()
	o.DiscretionaryAmt = r.ReadFloat()
	o.GoodAfterTime = r.ReadString()
	r.Skip(1) // deprecated sharesAllocation
	o.FAGroup = r.ReadString()
	o.FAMethod = r.ReadString()
	o.FAPercentage = r.ReadString()

	if ib.client.ServerVersion() < 177 {
		r.Skip(1) // faProfile (obsolete)
	}

	// Order fields (second block)
	o.ModelCode = r.ReadString()
	o.GoodTillDate = r.ReadString()
	o.Rule80A = r.ReadString()
	o.PercentOffset = r.ReadFloat()
	o.SettlingFirm = r.ReadString()
	o.ShortSaleSlot = int(r.ReadInt())
	o.DesignatedLocation = r.ReadString()
	o.ExemptCode = int(r.ReadInt())
	o.AuctionStrategy = int(r.ReadInt())
	o.StartingPrice = r.ReadFloat()
	o.StockRefPrice = r.ReadFloat()
	o.Delta = r.ReadFloat()
	o.StockRangeLower = r.ReadFloat()
	o.StockRangeUpper = r.ReadFloat()
	o.DisplaySize = int(r.ReadInt())
	o.BlockOrder = r.ReadBool()
	o.SweepToFill = r.ReadBool()
	o.AllOrNone = r.ReadBool()
	o.MinQty = r.ReadInt()
	o.OcaType = int(r.ReadInt())
	o.ETradeOnly = r.ReadBool()
	o.FirmQuoteOnly = r.ReadBool()
	o.NBBOPriceCap = r.ReadFloat()
	o.ParentID = r.ReadInt()
	o.TriggerMethod = int(r.ReadInt())
	o.Volatility = r.ReadFloat()
	o.VolatilityType = r.ReadInt()
	o.DeltaNeutralOrderType = r.ReadString()
	o.DeltaNeutralAuxPrice = r.ReadFloat()

	if o.DeltaNeutralOrderType != "" {
		o.DeltaNeutralConID = r.ReadInt()
		o.DeltaNeutralSettlingFirm = r.ReadString()
		o.DeltaNeutralClearingAccount = r.ReadString()
		o.DeltaNeutralClearingIntent = r.ReadString()
		o.DeltaNeutralOpenClose = r.ReadString()
		o.DeltaNeutralShortSale = r.ReadBool()
		o.DeltaNeutralShortSaleSlot = int(r.ReadInt())
		o.DeltaNeutralDesignatedLocation = r.ReadString()
	}

	o.ContinuousUpdate = r.ReadBool()
	o.ReferencePriceType = r.ReadInt()
	o.TrailStopPrice = r.ReadFloat()
	o.TrailingPercent = r.ReadFloat()
	o.BasisPoints = r.ReadFloat()
	o.BasisPointsType = r.ReadInt()
	c.ComboLegsDescrip = r.ReadString()

	// Combo legs
	numLegs := int(r.ReadInt())
	if numLegs > 0 {
		c.ComboLegs = make([]contract.ComboLeg, numLegs)
		for i := range numLegs {
			c.ComboLegs[i] = contract.ComboLeg{
				ConID:              r.ReadInt(),
				Ratio:              int(r.ReadInt()),
				Action:             r.ReadString(),
				Exchange:           r.ReadString(),
				OpenClose:          int(r.ReadInt()),
				ShortSaleSlot:      int(r.ReadInt()),
				DesignatedLocation: r.ReadString(),
				ExemptCode:         int(r.ReadInt()),
			}
		}
	}

	// Order combo legs
	numOrderLegs := int(r.ReadInt())
	if numOrderLegs > 0 {
		o.OrderComboLegs = make([]order.OrderComboLeg, numOrderLegs)
		for i := range numOrderLegs {
			o.OrderComboLegs[i] = order.OrderComboLeg{Price: r.ReadFloat()}
		}
	}

	// Smart combo routing params
	numParams := int(r.ReadInt())
	if numParams > 0 {
		o.SmartComboRoutingParams = make([]contract.TagValue, numParams)
		for i := range numParams {
			o.SmartComboRoutingParams[i] = contract.TagValue{
				Tag:   r.ReadString(),
				Value: r.ReadString(),
			}
		}
	}

	// Scale orders
	o.ScaleInitLevelSize = r.ReadInt()
	o.ScaleSubsLevelSize = r.ReadInt()
	scalePriceIncrement := r.ReadFloat()
	if scalePriceIncrement == 0 {
		scalePriceIncrement = UnsetDouble
	}
	o.ScalePriceIncrement = scalePriceIncrement

	if o.ScalePriceIncrement > 0 && o.ScalePriceIncrement < UnsetDouble {
		o.ScalePriceAdjustValue = r.ReadFloat()
		o.ScalePriceAdjustInterval = r.ReadInt()
		o.ScaleProfitOffset = r.ReadFloat()
		o.ScaleAutoReset = r.ReadBool()
		o.ScaleInitPosition = r.ReadInt()
		o.ScaleInitFillQty = r.ReadInt()
		o.ScaleRandomPercent = r.ReadBool()
	}

	// Hedge
	o.HedgeType = r.ReadString()
	if o.HedgeType != "" {
		o.HedgeParam = r.ReadString()
	}

	o.OptOutSmartRouting = r.ReadBool()
	o.ClearingAccount = r.ReadString()
	o.ClearingIntent = r.ReadString()
	o.NotHeld = r.ReadBool()

	// Delta neutral contract
	dncPresent := r.ReadBool()
	if dncPresent {
		c.DeltaNeutralContract = &contract.DeltaNeutralContract{
			ConID: r.ReadInt(),
			Delta: r.ReadFloat(),
			Price: r.ReadFloat(),
		}
	}

	// Algo
	o.AlgoStrategy = r.ReadString()
	if o.AlgoStrategy != "" {
		numAlgoParams := int(r.ReadInt())
		if numAlgoParams > 0 {
			o.AlgoParams = make([]contract.TagValue, numAlgoParams)
			for i := range numAlgoParams {
				o.AlgoParams[i] = contract.TagValue{
					Tag:   r.ReadString(),
					Value: r.ReadString(),
				}
			}
		}
	}

	// What-if & order state
	o.Solicited = r.ReadBool()
	o.WhatIf = r.ReadBool()
	st.Status = r.ReadString()
	st.InitMarginBefore = r.ReadString()
	st.MaintMarginBefore = r.ReadString()
	st.EquityWithLoanBefore = r.ReadString()
	st.InitMarginChange = r.ReadString()
	st.MaintMarginChange = r.ReadString()
	st.EquityWithLoanChange = r.ReadString()
	st.InitMarginAfter = r.ReadString()
	st.MaintMarginAfter = r.ReadString()
	st.EquityWithLoanAfter = r.ReadString()
	st.Commission = r.ReadFloat()
	st.MinCommission = r.ReadFloat()
	st.MaxCommission = r.ReadFloat()
	st.CommissionCurrency = r.ReadString()
	st.WarningText = r.ReadString()
	o.RandomizeSize = r.ReadBool()
	o.RandomizePrice = r.ReadBool()

	// PEG BENCH
	if o.OrderType == "PEG BENCH" || o.OrderType == "PEGBENCH" {
		o.ReferenceContractID = r.ReadInt()
		o.IsPeggedChangeAmountDecrease = r.ReadBool()
		o.PeggedChangeAmount = r.ReadFloat()
		o.ReferenceChangeAmount = r.ReadFloat()
		o.ReferenceExchangeID = r.ReadString()
	}

	// Conditions
	numConditions := int(r.ReadInt())
	if numConditions > 0 {
		o.Conditions = make([]order.OrderCondition, 0, numConditions)
		for range numConditions {
			condType := int(r.ReadInt())
			cond := order.CreateCondition(condType)
			if cond != nil {
				ib.readConditionFields(cond, r)
				o.Conditions = append(o.Conditions, cond)
			}
		}
		o.ConditionsIgnoreRTH = r.ReadBool()
		o.ConditionsCancelOrder = r.ReadBool()
	}

	// Adjusted orders
	o.AdjustedOrderType = r.ReadString()
	o.TriggerPrice = r.ReadFloat()
	o.TrailStopPrice = r.ReadFloat()
	o.LmtPriceOffset = r.ReadFloat()
	o.AdjustedStopPrice = r.ReadFloat()
	o.AdjustedStopLimitPrice = r.ReadFloat()
	o.AdjustedTrailingAmount = r.ReadFloat()
	o.AdjustableTrailingUnit = int(r.ReadInt())

	// Soft dollar tier
	o.SoftDollarTier = order.SoftDollarTier{
		Name:        r.ReadString(),
		Val:         r.ReadString(),
		DisplayName: r.ReadString(),
	}

	o.CashQty = r.ReadFloat()
	o.DontUseAutoPriceForHedge = r.ReadBool()
	o.IsOmsContainer = r.ReadBool()
	o.DiscretionaryUpToLimitPrice = r.ReadBool()
	o.UsePriceMgmtAlgo = r.ReadBool()

	if ib.client.ServerVersion() >= 159 {
		o.Duration = r.ReadInt()
	}
	if ib.client.ServerVersion() >= 160 {
		o.PostToAts = r.ReadInt()
	}
	if ib.client.ServerVersion() >= 162 {
		o.AutoCancelParent = r.ReadBool()
	}
	if ib.client.ServerVersion() >= 170 {
		o.MinTradeQty = r.ReadInt()
		o.MinCompeteSize = r.ReadInt()
		o.CompeteAgainstBestOffset = r.ReadFloat()
		o.MidOffsetAtWhole = r.ReadFloat()
		o.MidOffsetAtHalf = r.ReadFloat()
	}

	if o.WhatIf {
		// Response to WhatIfOrder: never a real trade. TWS may send a first
		// message without margin numbers; complete on the one that has them.
		if v, err := strconv.ParseFloat(st.InitMarginChange, 64); err == nil && v != UnsetDouble {
			ib.state.EndReq(o.OrderID, st, nil)
		}
		ib.client.UpdateReqID(o.OrderID + 1)
		return
	}

	// Update trade state
	key := orderKeyFromOrder(o)
	ib.state.Mu.Lock()
	trade, exists := ib.state.Trades[key]
	if !exists && o.PermID != 0 {
		trade, exists = ib.state.PermID2Trade[o.PermID], ib.state.PermID2Trade[o.PermID] != nil
	}
	if !exists {
		// New trade from TWS or another client
		os := &order.OrderStatus{OrderID: o.OrderID, Status: st.Status, PermID: o.PermID, ClientID: o.ClientID}
		trade = &order.Trade{Contract: c, Order: o, OrderStatus: os}
		ib.state.Trades[key] = trade
	}
	if o.PermID != 0 {
		if _, ok := ib.state.PermID2Trade[o.PermID]; !ok {
			ib.state.PermID2Trade[o.PermID] = trade
		}
	}
	ib.state.Mu.Unlock()
	if exists {
		trade.UpdateFromOpenOrder(o)
	}

	ib.client.UpdateReqID(o.OrderID + 1)

	// During ReqOpenOrders/ReqAllOpenOrders the trade goes to the result
	// list; otherwise it is an unsolicited update.
	if reqKey := ib.openOrdersReq.active(); reqKey != 0 && ib.state.HasResult(reqKey) {
		ib.state.AppendResult(reqKey, trade)
	} else {
		ib.OpenOrderEvent.Emit(trade)
	}
}

func orderKeyFromOrder(o *order.Order) state.OrderKey {
	if o.OrderID <= 0 {
		// Manual TWS order — use permId as key
		return state.OrderKey{ClientID: 0, OrderID: o.PermID}
	}
	return state.OrderKey{ClientID: o.ClientID, OrderID: o.OrderID}
}

func (ib *IB) readConditionFields(cond order.OrderCondition, r *protocol.FieldReader) {
	switch c := cond.(type) {
	case *order.PriceCondition:
		c.Conjunction = r.ReadString()
		c.IsMore = r.ReadBool()
		c.Price = r.ReadFloat()
		c.ConID = r.ReadInt()
		c.Exchange = r.ReadString()
		c.TriggerMethod = int(r.ReadInt())
	case *order.TimeCondition:
		c.Conjunction = r.ReadString()
		c.IsMore = r.ReadBool()
		c.Time = r.ReadString()
	case *order.MarginCondition:
		c.Conjunction = r.ReadString()
		c.IsMore = r.ReadBool()
		c.Percent = int(r.ReadInt())
	case *order.ExecutionCondition:
		c.Conjunction = r.ReadString()
		c.SecType = r.ReadString()
		c.Exchange = r.ReadString()
		c.Symbol = r.ReadString()
	case *order.VolumeCondition:
		c.Conjunction = r.ReadString()
		c.IsMore = r.ReadBool()
		c.Volume = int(r.ReadInt())
		c.ConID = r.ReadInt()
		c.Exchange = r.ReadString()
	case *order.PercentChangeCondition:
		c.Conjunction = r.ReadString()
		c.IsMore = r.ReadBool()
		c.ChangePercent = r.ReadFloat()
		c.ConID = r.ReadInt()
		c.Exchange = r.ReadString()
	}
}

func (ib *IB) processDividendTick(ticker *market.Ticker, value string) {
	// Format: "past12,next12,nextDate,nextAmount"
	parts := strings.Split(value, ",")
	if len(parts) < 4 {
		return
	}

	var past12, next12, nextAmount *float64
	if parts[0] != "" {
		if v, err := strconv.ParseFloat(parts[0], 64); err == nil {
			past12 = &v
		}
	}
	if parts[1] != "" {
		if v, err := strconv.ParseFloat(parts[1], 64); err == nil {
			next12 = &v
		}
	}
	if parts[3] != "" {
		if v, err := strconv.ParseFloat(parts[3], 64); err == nil {
			nextAmount = &v
		}
	}

	ticker.Dividends = &market.Dividends{
		Past12Months: past12,
		Next12Months: next12,
		NextDate:     parts[2],
		NextAmount:   nextAmount,
	}
}

func (ib *IB) processFundamentalRatios(ticker *market.Ticker, value string) {
	ratios := make(map[string]string)
	for _, pair := range strings.Split(value, ";") {
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			ratios[kv[0]] = kv[1]
		}
	}
	ticker.FundamentalRatios = ratios
}

// setTickerFloat sets a named float64 field on a Ticker.
func setTickerFloat(t *market.Ticker, field string, value float64) {
	switch field {
	case "Bid":
		t.Bid = value
	case "Ask":
		t.Ask = value
	case "Last":
		t.Last = value
	case "High":
		t.High = value
	case "Low":
		t.Low = value
	case "Open":
		t.Open = value
	case "Close":
		t.Close = value
	case "Volume":
		t.Volume = value
	case "VWAP":
		t.VWAP = value
	case "BidSize":
		t.BidSize = value
	case "AskSize":
		t.AskSize = value
	case "LastSize":
		t.LastSize = value
	case "AvVolume":
		t.AvVolume = value
	case "OpenInterest":
		t.OpenInterest = value
	case "HistVolatility":
		t.HistVolatility = value
	case "ImpliedVolatility":
		t.ImpliedVolatility = value
	case "MarkPrice":
		t.MarkPrice = value
	case "Halted":
		t.Halted = value
	case "TradeCount":
		t.TradeCount = value
	case "TradeRate":
		t.TradeRate = value
	case "VolumeRate":
		t.VolumeRate = value
	case "VolumeRate3Min":
		t.VolumeRate3Min = value
	case "VolumeRate5Min":
		t.VolumeRate5Min = value
	case "VolumeRate10Min":
		t.VolumeRate10Min = value
	case "Shortable":
		t.Shortable = value
	case "ShortableShares":
		t.ShortableShares = value
	case "IndexFuturePremium":
		t.IndexFuturePremium = value
	case "FuturesOpenInterest":
		t.FuturesOpenInterest = value
	case "CallOpenInterest":
		t.CallOpenInterest = value
	case "PutOpenInterest":
		t.PutOpenInterest = value
	case "CallVolume":
		t.CallVolume = value
	case "PutVolume":
		t.PutVolume = value
	case "AvOptionVolume":
		t.AvOptionVolume = value
	case "AuctionVolume":
		t.AuctionVolume = value
	case "AuctionPrice":
		t.AuctionPrice = value
	case "AuctionImbalance":
		t.AuctionImbalance = value
	case "RegulatoryImbalance":
		t.RegulatoryImbalance = value
	case "RTHistVolatility":
		t.RTHistVolatility = value
	case "BondFactorMultiplier":
		t.BondFactorMultiplier = value
	case "DelayedHalted":
		t.DelayedHalted = value
	case "Low13Week":
		t.Low13Week = value
	case "High13Week":
		t.High13Week = value
	case "Low26Week":
		t.Low26Week = value
	case "High26Week":
		t.High26Week = value
	case "Low52Week":
		t.Low52Week = value
	case "High52Week":
		t.High52Week = value
	case "BidYield":
		t.BidYield = value
	case "AskYield":
		t.AskYield = value
	case "LastYield":
		t.LastYield = value
	case "LastRthTrade":
		t.LastRthTrade = value
	case "CreditmanMarkPrice":
		t.CreditmanMarkPrice = value
	case "CreditmanSlowMarkPrice":
		t.CreditmanSlowMarkPrice = value
	case "EtfNavClose":
		t.EtfNavClose = value
	case "EtfNavPriorClose":
		t.EtfNavPriorClose = value
	case "EtfNavBid":
		t.EtfNavBid = value
	case "EtfNavAsk":
		t.EtfNavAsk = value
	case "EtfNavLast":
		t.EtfNavLast = value
	case "EtfFrozenNavLast":
		t.EtfFrozenNavLast = value
	case "EtfNavHigh":
		t.EtfNavHigh = value
	case "EtfNavLow":
		t.EtfNavLow = value
	case "EstimatedIpoMidpoint":
		t.EstimatedIpoMidpoint = value
	case "FinalIpoLast":
		t.FinalIpoLast = value
	}
}

// setTickerString sets a named string field on a Ticker.
func setTickerString(t *market.Ticker, field, value string) {
	switch field {
	case "BidExchange":
		t.BidExchange = value
	case "AskExchange":
		t.AskExchange = value
	case "LastExchange":
		t.LastExchange = value
	case "LastRegTime":
		t.LastRegTime = value
	case "OptionBidExch":
		t.OptionBidExch = value
	case "OptionAskExch":
		t.OptionAskExch = value
	case "ReutersMutualFunds":
		t.ReutersMutualFunds = value
	case "SocialMarketAnalytics":
		t.SocialMarketAnalytics = value
	}
}

// --- Historical Data ---

// --- ReqHistoricalData public method ---

// ReqHistoricalData requests historical bar data.
func (ib *IB) ReqHistoricalData(ctx context.Context, con *contract.Contract, endDateTime, durationStr, barSizeSetting, whatToShow string, useRTH bool, formatDate int) ([]market.BarData, error) {
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, con)

	if err := ib.client.ReqHistoricalData(reqID, con, endDateTime, durationStr, barSizeSetting, whatToShow, useRTH, formatDate, false, nil); err != nil {
		ib.state.CancelReq(reqID)
		return nil, err
	}

	v, err := ib.awaitReq(ctx, reqID, ch)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_ = ib.client.CancelHistoricalData(reqID)
		}
		return nil, err
	}
	bars, _ := v.([]market.BarData)
	return bars, nil
}

// --- ReqMktData subscription ---

// --- PlaceOrder ---

// PlaceOrder submits a new order, or modifies a live one when ord.OrderID
// belongs to an existing trade of this client. It returns the Trade that
// tracks the order lifecycle.
//
// Before anything is sent, the installed risk manager (SetRiskManager) must
// accept the order and the installed journal (SetJournal) must durably record
// it; either failing aborts the order.
func (ib *IB) PlaceOrder(con *contract.Contract, ord *order.Order) (*order.Trade, error) {
	if con == nil || ord == nil {
		return nil, errors.New("ibgo: PlaceOrder needs a contract and an order")
	}
	if ord.WhatIf {
		return nil, ErrUseWhatIfOrder
	}
	ib.placeMu.Lock()
	defer ib.placeMu.Unlock()
	return ib.placeOrderLocked(con, ord, true)
}

// placeOrderLocked implements PlaceOrder; placeMu must be held.
func (ib *IB) placeOrderLocked(con *contract.Contract, ord *order.Order, runRisk bool) (*order.Trade, error) {
	if !ib.client.IsConnected() {
		return nil, ErrNotConnected
	}
	clientID := ib.ClientID()
	if ord.OrderID == 0 {
		ord.OrderID = ib.client.GetReqID()
	}
	key := orderKeyFromIDs(clientID, ord.OrderID, ord.PermID)

	ib.state.Mu.RLock()
	existing := ib.state.Trades[key]
	ib.state.Mu.RUnlock()
	isModify := existing != nil
	if isModify && existing.IsDone() {
		return existing, ErrOrderDone
	}
	if !isModify {
		ord.ClientID = clientID
	}

	if runRisk {
		if rc := ib.riskChecker(); rc != nil {
			if err := rc.CheckOrder(con, ord, isModify); err != nil {
				return nil, err
			}
		}
	}

	// IB API BUG FIX: strip volatility for non-VOL orders
	if !strings.HasPrefix(ord.OrderType, "VOL") {
		ord.Volatility = UnsetDouble
	}

	// Write-ahead: an order that cannot be journaled is not sent.
	if j := ib.orderJournal(); j != nil {
		if err := j.RecordSubmit(con, ord, isModify); err != nil {
			return nil, fmt.Errorf("ibgo: journal write failed, order not sent: %w", err)
		}
	}

	now := time.Now()
	var trade *order.Trade
	if isModify {
		trade = existing
		// Logged before sending so the "Submitted" acknowledgement is
		// recognised as "Modified" even if it arrives immediately.
		trade.AppendLog(order.TradeLogEntry{Time: now, Status: trade.Status(), Message: "Modify"})
	} else {
		trade = &order.Trade{
			Contract: con,
			Order:    ord,
			OrderStatus: &order.OrderStatus{
				OrderID:  ord.OrderID,
				ClientID: clientID,
				Status:   order.StatusPendingSubmit,
			},
			Log: []order.TradeLogEntry{{Time: now, Status: order.StatusPendingSubmit}},
		}
		// Registered before sending so immediate responses find the trade.
		ib.state.Mu.Lock()
		ib.state.Trades[key] = trade
		ib.state.Mu.Unlock()
	}

	if err := ib.sendPlaceOrder(ord.OrderID, con, ord); err != nil {
		if isModify {
			trade.AppendLog(order.TradeLogEntry{Time: time.Now(), Status: trade.Status(), Message: "Modify failed: " + err.Error()})
		} else {
			ib.state.Mu.Lock()
			delete(ib.state.Trades, key)
			ib.state.Mu.Unlock()
		}
		ib.journalError(trade, 0, "send failed: "+err.Error())
		return nil, err
	}

	if isModify {
		trade.ModifyEvent.Emit(trade)
		ib.OrderModifyEvent.Emit(trade)
	} else {
		ib.NewOrderEvent.Emit(trade)
	}
	return trade, nil
}

// sendPlaceOrder encodes and sends a placeOrder message.
func (ib *IB) sendPlaceOrder(orderID int64, con *contract.Contract, ord *order.Order) error {
	version := ib.client.ServerVersion()
	fields := []interface{}{
		protocol.MsgPlaceOrder,
		orderID,
		con,
		con.SecIDType, con.SecID,
		ord.Action, ord.TotalQuantity, ord.OrderType,
		ord.LmtPrice, ord.AuxPrice, ord.TIF,
		ord.OcaGroup, ord.Account, ord.OpenClose,
		ord.Origin, ord.OrderRef, ord.Transmit,
		ord.ParentID, ord.BlockOrder, ord.SweepToFill,
		ord.DisplaySize, ord.TriggerMethod, ord.OutsideRTH, ord.Hidden,
	}

	// BAG combo legs
	if con.SecType == "BAG" {
		legs := con.ComboLegs
		fields = append(fields, len(legs))
		for _, leg := range legs {
			fields = append(fields, leg.ConID, leg.Ratio, leg.Action, leg.Exchange,
				leg.OpenClose, leg.ShortSaleSlot, leg.DesignatedLocation, leg.ExemptCode)
		}
		oLegs := ord.OrderComboLegs
		fields = append(fields, len(oLegs))
		for _, leg := range oLegs {
			fields = append(fields, leg.Price)
		}
		params := ord.SmartComboRoutingParams
		fields = append(fields, len(params))
		for _, p := range params {
			fields = append(fields, p.Tag, p.Value)
		}
	}

	fields = append(fields,
		"", // deprecated sharesAllocation
		ord.DiscretionaryAmt, ord.GoodAfterTime, ord.GoodTillDate,
		ord.FAGroup, ord.FAMethod, ord.FAPercentage,
	)

	if version < 177 {
		fields = append(fields, "") // faProfile (obsolete)
	}

	fields = append(fields,
		ord.ModelCode, ord.ShortSaleSlot, ord.DesignatedLocation,
		ord.ExemptCode, ord.OcaType, ord.Rule80A, ord.SettlingFirm,
		ord.AllOrNone, ord.MinQty, ord.PercentOffset,
		ord.ETradeOnly, ord.FirmQuoteOnly, ord.NBBOPriceCap,
		ord.AuctionStrategy, ord.StartingPrice, ord.StockRefPrice,
		ord.Delta, ord.StockRangeLower, ord.StockRangeUpper,
		ord.OverridePercentageConstraints,
		ord.Volatility, ord.VolatilityType,
		ord.DeltaNeutralOrderType, ord.DeltaNeutralAuxPrice,
	)

	if ord.DeltaNeutralOrderType != "" {
		fields = append(fields,
			ord.DeltaNeutralConID, ord.DeltaNeutralSettlingFirm,
			ord.DeltaNeutralClearingAccount, ord.DeltaNeutralClearingIntent,
			ord.DeltaNeutralOpenClose, ord.DeltaNeutralShortSale,
			ord.DeltaNeutralShortSaleSlot, ord.DeltaNeutralDesignatedLocation,
		)
	}

	fields = append(fields,
		ord.ContinuousUpdate, ord.ReferencePriceType,
		ord.TrailStopPrice, ord.TrailingPercent,
		ord.ScaleInitLevelSize, ord.ScaleSubsLevelSize, ord.ScalePriceIncrement,
	)

	if ord.ScalePriceIncrement > 0 && ord.ScalePriceIncrement < UnsetDouble {
		fields = append(fields,
			ord.ScalePriceAdjustValue, ord.ScalePriceAdjustInterval,
			ord.ScaleProfitOffset, ord.ScaleAutoReset,
			ord.ScaleInitPosition, ord.ScaleInitFillQty, ord.ScaleRandomPercent,
		)
	}

	fields = append(fields, ord.ScaleTable, ord.ActiveStartTime, ord.ActiveStopTime, ord.HedgeType)
	if ord.HedgeType != "" {
		fields = append(fields, ord.HedgeParam)
	}

	fields = append(fields, ord.OptOutSmartRouting, ord.ClearingAccount, ord.ClearingIntent, ord.NotHeld)

	if con.DeltaNeutralContract != nil {
		dnc := con.DeltaNeutralContract
		fields = append(fields, true, dnc.ConID, dnc.Delta, dnc.Price)
	} else {
		fields = append(fields, false)
	}

	fields = append(fields, ord.AlgoStrategy)
	if ord.AlgoStrategy != "" {
		fields = append(fields, len(ord.AlgoParams))
		for _, p := range ord.AlgoParams {
			fields = append(fields, p.Tag, p.Value)
		}
	}

	fields = append(fields, ord.AlgoID, ord.WhatIf, ord.OrderMiscOptions,
		ord.Solicited, ord.RandomizeSize, ord.RandomizePrice,
	)

	if ord.OrderType == "PEG BENCH" || ord.OrderType == "PEGBENCH" {
		fields = append(fields,
			ord.ReferenceContractID, ord.IsPeggedChangeAmountDecrease,
			ord.PeggedChangeAmount, ord.ReferenceChangeAmount, ord.ReferenceExchangeID,
		)
	}

	fields = append(fields, len(ord.Conditions))
	if len(ord.Conditions) > 0 {
		for _, cond := range ord.Conditions {
			fields = append(fields, encodeCondition(cond)...)
		}
		fields = append(fields, ord.ConditionsIgnoreRTH, ord.ConditionsCancelOrder)
	}

	fields = append(fields,
		ord.AdjustedOrderType, ord.TriggerPrice, ord.LmtPriceOffset,
		ord.AdjustedStopPrice, ord.AdjustedStopLimitPrice,
		ord.AdjustedTrailingAmount, ord.AdjustableTrailingUnit,
		ord.ExtOperator,
		ord.SoftDollarTier.Name, ord.SoftDollarTier.Val,
		ord.CashQty,
		ord.Mifid2DecisionMaker, ord.Mifid2DecisionAlgo,
		ord.Mifid2ExecutionTrader, ord.Mifid2ExecutionAlgo,
		ord.DontUseAutoPriceForHedge, ord.IsOmsContainer,
		ord.DiscretionaryUpToLimitPrice, ord.UsePriceMgmtAlgo,
	)

	if version >= 158 {
		fields = append(fields, ord.Duration)
	}
	if version >= 160 {
		fields = append(fields, ord.PostToAts)
	}
	if version >= 162 {
		fields = append(fields, ord.AutoCancelParent)
	}
	if version >= 166 {
		fields = append(fields, ord.AdvancedErrorOverride)
	}
	if version >= 169 {
		fields = append(fields, ord.ManualOrderTime)
	}
	if version >= 170 {
		if con.Exchange == "IBKRATS" {
			fields = append(fields, ord.MinTradeQty)
		}
		if ord.OrderType == "PEG BEST" || ord.OrderType == "PEGBEST" {
			fields = append(fields, ord.MinCompeteSize, ord.CompeteAgainstBestOffset)
			if ord.CompeteAgainstBestOffset == math.Inf(1) {
				fields = append(fields, ord.MidOffsetAtWhole, ord.MidOffsetAtHalf)
			}
		} else if ord.OrderType == "PEG MID" || ord.OrderType == "PEGMID" {
			fields = append(fields, ord.MidOffsetAtWhole, ord.MidOffsetAtHalf)
		}
	}

	return ib.client.Send(fields...)
}

func encodeCondition(cond order.OrderCondition) []interface{} {
	switch c := cond.(type) {
	case *order.PriceCondition:
		return []interface{}{c.CondType(), c.Conjunction, c.IsMore, c.Price, c.ConID, c.Exchange, c.TriggerMethod}
	case *order.TimeCondition:
		return []interface{}{c.CondType(), c.Conjunction, c.IsMore, c.Time}
	case *order.MarginCondition:
		return []interface{}{c.CondType(), c.Conjunction, c.IsMore, c.Percent}
	case *order.ExecutionCondition:
		return []interface{}{c.CondType(), c.Conjunction, c.SecType, c.Exchange, c.Symbol}
	case *order.VolumeCondition:
		return []interface{}{c.CondType(), c.Conjunction, c.IsMore, c.Volume, c.ConID, c.Exchange}
	case *order.PercentChangeCondition:
		return []interface{}{c.CondType(), c.Conjunction, c.IsMore, c.ChangePercent, c.ConID, c.Exchange}
	default:
		return nil
	}
}

// CancelOrder requests cancellation of an order and returns the Trade it
// belongs to (nil if unknown). The trade moves to PendingCancel (or straight
// to Cancelled for an untransmitted/inactive order) until TWS confirms.
func (ib *IB) CancelOrder(ord *order.Order) (*order.Trade, error) {
	if ord == nil || ord.OrderID <= 0 {
		return nil, errors.New("ibgo: CancelOrder needs an order with an orderId")
	}
	clientID := ord.ClientID
	if clientID == 0 {
		clientID = ib.ClientID()
	}
	ib.state.Mu.RLock()
	trade := ib.state.Trades[orderKeyFromIDs(clientID, ord.OrderID, ord.PermID)]
	if trade == nil && ord.PermID != 0 {
		trade = ib.state.PermID2Trade[ord.PermID]
	}
	ib.state.Mu.RUnlock()

	if trade != nil {
		if j := ib.orderJournal(); j != nil {
			// Cancels reduce risk: a journal failure is logged, never blocking.
			logJournalErr("cancel", j.RecordCancelRequest(trade))
		}
	}

	if err := ib.client.CancelOrder(ord.OrderID, ""); err != nil {
		return trade, err
	}

	if trade != nil && !trade.IsDone() {
		status := trade.Status()
		newStatus := order.StatusPendingCancel
		if (status == order.StatusPendingSubmit && !trade.Order.Transmit) || status == order.StatusInactive {
			newStatus = order.StatusCancelled
		}
		trade.SetStatus(newStatus, time.Now(), "", 0)
		ib.journalStatus(trade)
		trade.CancelEvent.Emit(trade)
		trade.StatusEvent.Emit(trade)
		ib.CancelOrderEvent.Emit(trade)
		ib.OrderStatusEvent.Emit(trade)
		if newStatus == order.StatusCancelled {
			trade.CancelledEvent.Emit(trade)
		}
	}
	return trade, nil
}

// ReqGlobalCancel cancels all active orders, including those placed by other
// clients or directly in TWS.
func (ib *IB) ReqGlobalCancel() error {
	return ib.client.ReqGlobalCancel()
}

// --- Additional Handlers ---

func (ib *IB) handleOpenOrderEnd(r *protocol.FieldReader) {
	if key := ib.openOrdersReq.active(); key != 0 {
		ib.state.EndReq(key, nil, nil)
	}
}

func (ib *IB) handleExecDetails(r *protocol.FieldReader) {
	// Layout (server >= 136, no version field):
	//   reqId, orderId, <contract>, execId, time, acctNumber, exchange, side, ...
	// The msgId was already consumed by the reader.
	reqID := r.ReadInt()

	exec := &order.Execution{}
	c := &contract.Contract{}

	// Order ID first
	exec.OrderID = r.ReadInt()

	// Embedded contract fields
	c.ConID = r.ReadInt()
	c.Symbol = r.ReadString()
	c.SecType = r.ReadString()
	c.LastTradeDateOrContractMonth = r.ReadString()
	c.Strike = r.ReadFloat()
	c.Right = r.ReadString()
	c.Multiplier = r.ReadString()
	c.Exchange = r.ReadString()
	c.Currency = r.ReadString()
	c.LocalSymbol = r.ReadString()
	c.TradingClass = r.ReadString()

	// Execution fields
	exec.ExecID = r.ReadString()
	exec.Time = parseBarDate(r.ReadString())
	exec.AcctNumber = r.ReadString()
	exec.Exchange = r.ReadString()
	exec.Side = r.ReadString()
	exec.Shares = r.ReadFloat()
	exec.Price = r.ReadFloat()
	exec.PermID = r.ReadInt()
	exec.ClientID = r.ReadInt()
	exec.Liquidation = int(r.ReadInt())
	exec.CumQty = r.ReadFloat()
	exec.AvgPrice = r.ReadFloat()
	exec.OrderRef = r.ReadString()
	exec.EvRule = r.ReadString()
	exec.EvMultiplier = r.ReadFloat()
	exec.ModelCode = r.ReadString()
	exec.LastLiquidity = int(r.ReadInt())

	if ib.client.ServerVersion() >= 178 {
		exec.PendingPriceRevision = r.ReadBool()
	}

	// IB TWS bug: manual order executions have orderId == UNSET_INTEGER
	if exec.OrderID == int64(math.MaxInt32) {
		exec.OrderID = 0
	}

	// Live executions arrive unsolicited; reqExecutions responses carry the
	// request id.
	isLive := !ib.state.Requests.Has(reqID)
	fillTime := exec.Time
	if isLive {
		fillTime = ib.now()
	}
	fill := &order.Fill{
		Contract:         c,
		Execution:        exec,
		CommissionReport: &order.CommissionReport{},
		Time:             fillTime,
	}

	ib.state.Mu.Lock()
	// First try permId, then orderKey
	trade := ib.state.PermID2Trade[exec.PermID]
	if trade == nil {
		trade = ib.state.Trades[orderKeyForExec(exec)]
	}
	isNew := false
	if existing, exists := ib.state.Fills[exec.ExecID]; exists {
		fill = existing
	} else {
		ib.state.Fills[exec.ExecID] = fill
		isNew = true
	}
	ib.state.Mu.Unlock()

	if isNew {
		if trade != nil {
			trade.AddFill(fill, fillTime)
		}
		ib.journalFill(trade, fill)
		// Events only for the first sighting of an execution (TWS resends
		// executions, e.g. after a reconnect).
		if isLive && trade != nil {
			ib.ExecDetailsEvent.Emit(fill)
			trade.FillEvent.Emit(fill)
		}
	}

	// For reqExecutions responses, accumulate results
	if !isLive {
		ib.state.AppendResult(reqID, fill)
	}
}

func orderKeyForExec(exec *order.Execution) state.OrderKey {
	if exec.OrderID <= 0 {
		return state.OrderKey{ClientID: 0, OrderID: exec.PermID}
	}
	return state.OrderKey{ClientID: exec.ClientID, OrderID: exec.OrderID}
}

func (ib *IB) handleExecDetailsEnd(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	ib.state.EndReq(reqID, nil, nil)
}

func (ib *IB) handleCommissionReport(r *protocol.FieldReader) {
	r.Skip(1) // version
	cr := &order.CommissionReport{
		ExecID:              r.ReadString(),
		Commission:          r.ReadFloat(),
		Currency:            r.ReadString(),
		RealizedPNL:         r.ReadFloat(),
		Yield:               r.ReadFloat(),
		YieldRedemptionDate: int(r.ReadInt()),
	}

	if cr.RealizedPNL == math.MaxFloat64 {
		cr.RealizedPNL = 0
	}
	if cr.Yield == math.MaxFloat64 {
		cr.Yield = 0
	}

	ib.state.Mu.Lock()
	fill, ok := ib.state.Fills[cr.ExecID]
	var trade *order.Trade
	if ok {
		fill.CommissionReport = cr
		trade = ib.state.PermID2Trade[fill.Execution.PermID]
		if trade == nil {
			trade = ib.state.Trades[orderKeyForExec(fill.Execution)]
		}
	}
	ib.state.Mu.Unlock()

	if !ok {
		return // commission report for an execution this client never saw
	}
	ib.notifyCommission(trade, fill)
	ib.CommissionReportEvent.Emit(fill)
	if trade != nil {
		trade.CommissionReportEvent.Emit(fill)
	}
}

func (ib *IB) handleUpdatePortfolio(r *protocol.FieldReader) {
	r.Skip(1) // version
	c := &contract.Contract{
		ConID:                        r.ReadInt(),
		Symbol:                       r.ReadString(),
		SecType:                      r.ReadString(),
		LastTradeDateOrContractMonth: r.ReadString(),
		Strike:                       r.ReadFloat(),
		Right:                        r.ReadString(),
		Multiplier:                   r.ReadString(),
		PrimaryExchange:              r.ReadString(),
		Currency:                     r.ReadString(),
		LocalSymbol:                  r.ReadString(),
		TradingClass:                 r.ReadString(),
	}
	position := r.ReadFloat()
	marketPrice := r.ReadFloat()
	marketValue := r.ReadFloat()
	averageCost := r.ReadFloat()
	unrealizedPNL := r.ReadFloat()
	realizedPNL := r.ReadFloat()
	acctName := r.ReadString()

	item := &account.PortfolioItem{
		Contract:      c,
		Position:      position,
		MarketPrice:   marketPrice,
		MarketValue:   marketValue,
		AverageCost:   averageCost,
		UnrealizedPNL: unrealizedPNL,
		RealizedPNL:   realizedPNL,
		Account:       acctName,
	}

	ib.state.Mu.Lock()
	if _, ok := ib.state.Portfolio[acctName]; !ok {
		ib.state.Portfolio[acctName] = make(map[int64]*account.PortfolioItem)
	}
	if position == 0 {
		delete(ib.state.Portfolio[acctName], c.ConID)
	} else {
		ib.state.Portfolio[acctName][c.ConID] = item
	}
	ib.state.Mu.Unlock()
	ib.noteAcctDownload(acctName, c.ConID)

	cp := *item
	ib.PortfolioEvent.Emit(&cp)
}

func (ib *IB) handleSecDefOptParams(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	exchange := r.ReadString()
	underlyingConID := r.ReadInt()
	tradingClass := r.ReadString()
	multiplier := r.ReadString()

	numExpirations := int(r.ReadInt())
	expirations := make([]string, numExpirations)
	for i := range numExpirations {
		expirations[i] = r.ReadString()
	}

	numStrikes := int(r.ReadInt())
	strikes := make([]float64, numStrikes)
	for i := range numStrikes {
		strikes[i] = r.ReadFloat()
	}

	chain := &market.OptionChain{
		Exchange:        exchange,
		UnderlyingConID: underlyingConID,
		TradingClass:    tradingClass,
		Multiplier:      multiplier,
		Expirations:     expirations,
		Strikes:         strikes,
	}

	ib.state.AppendResult(reqID, chain)
}

func (ib *IB) handleSecDefOptParamsEnd(r *protocol.FieldReader) {
	reqID := r.ReadInt()
	ib.state.EndReq(reqID, nil, nil)
}

// --- Additional Public Request Methods ---

// ReqSecDefOptParams requests option chain definition.
func (ib *IB) ReqSecDefOptParams(ctx context.Context, underlyingSymbol, futFopExchange, underlyingSecType string, underlyingConID int64) ([]market.OptionChain, error) {
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, nil)

	if err := ib.client.ReqSecDefOptParams(reqID, underlyingSymbol, futFopExchange, underlyingSecType, underlyingConID); err != nil {
		ib.state.CancelReq(reqID)
		return nil, err
	}

	v, err := ib.awaitReq(ctx, reqID, ch)
	if err != nil {
		return nil, err
	}
	items := resultItems(v)
	chains := make([]market.OptionChain, 0, len(items))
	for _, item := range items {
		if c, ok := item.(*market.OptionChain); ok {
			chains = append(chains, *c)
		}
	}
	return chains, nil
}

// ReqCompletedOrders requests all completed (filled/cancelled) orders.
// Serialized: IB sends one completedOrdersEnd for all outstanding requests.
func (ib *IB) ReqCompletedOrders(ctx context.Context, apiOnly bool) ([]*order.Trade, error) {
	v, err := ib.runSingle(ctx, ib.completedReq, true, func() error {
		return ib.client.ReqCompletedOrders(apiOnly)
	})
	if err != nil {
		return nil, err
	}
	items := resultItems(v)
	trades := make([]*order.Trade, 0, len(items))
	for _, item := range items {
		if t, ok := item.(*order.Trade); ok {
			trades = append(trades, t)
		}
	}
	return trades, nil
}

// --- completedOrder / orderBound / currentTime handlers ---

func (ib *IB) handleCompletedOrder(r *protocol.FieldReader) {
	o := order.NewOrder()
	c := &contract.Contract{}
	st := order.NewOrderState()

	// completedOrder has skip=1 (no version field, just msgId already skipped)
	c.ConID = r.ReadInt()
	c.Symbol = r.ReadString()
	c.SecType = r.ReadString()
	c.LastTradeDateOrContractMonth = r.ReadString()
	c.Strike = r.ReadFloat()
	c.Right = r.ReadString()
	c.Multiplier = r.ReadString()
	c.Exchange = r.ReadString()
	c.Currency = r.ReadString()
	c.LocalSymbol = r.ReadString()
	c.TradingClass = r.ReadString()

	o.Action = r.ReadString()
	o.TotalQuantity = r.ReadFloat()
	o.OrderType = r.ReadString()
	o.LmtPrice = r.ReadFloat()
	o.AuxPrice = r.ReadFloat()
	o.TIF = r.ReadString()
	o.OcaGroup = r.ReadString()
	o.Account = r.ReadString()
	o.OpenClose = r.ReadString()
	o.Origin = int(r.ReadInt())
	o.OrderRef = r.ReadString()
	o.PermID = r.ReadInt()
	o.OutsideRTH = r.ReadBool()
	o.Hidden = r.ReadBool()
	o.DiscretionaryAmt = r.ReadFloat()
	o.GoodAfterTime = r.ReadString()
	o.FAGroup = r.ReadString()
	o.FAMethod = r.ReadString()
	o.FAPercentage = r.ReadString()

	if ib.client.ServerVersion() < 177 {
		r.Skip(1) // faProfile
	}

	o.ModelCode = r.ReadString()
	o.GoodTillDate = r.ReadString()
	o.Rule80A = r.ReadString()
	o.PercentOffset = r.ReadFloat()
	o.SettlingFirm = r.ReadString()
	o.ShortSaleSlot = int(r.ReadInt())
	o.DesignatedLocation = r.ReadString()
	o.ExemptCode = int(r.ReadInt())
	o.StartingPrice = r.ReadFloat()
	o.StockRefPrice = r.ReadFloat()
	o.Delta = r.ReadFloat()
	o.StockRangeLower = r.ReadFloat()
	o.StockRangeUpper = r.ReadFloat()
	o.DisplaySize = int(r.ReadInt())
	o.SweepToFill = r.ReadBool()
	o.AllOrNone = r.ReadBool()
	o.MinQty = r.ReadInt()
	o.OcaType = int(r.ReadInt())
	o.TriggerMethod = int(r.ReadInt())
	o.Volatility = r.ReadFloat()
	o.VolatilityType = r.ReadInt()
	o.DeltaNeutralOrderType = r.ReadString()
	o.DeltaNeutralAuxPrice = r.ReadFloat()

	if o.DeltaNeutralOrderType != "" {
		o.DeltaNeutralConID = r.ReadInt()
		o.DeltaNeutralShortSale = r.ReadBool()
		o.DeltaNeutralShortSaleSlot = int(r.ReadInt())
		o.DeltaNeutralDesignatedLocation = r.ReadString()
	}

	o.ContinuousUpdate = r.ReadBool()
	o.ReferencePriceType = r.ReadInt()
	o.TrailStopPrice = r.ReadFloat()
	o.TrailingPercent = r.ReadFloat()
	c.ComboLegsDescrip = r.ReadString()

	// Combo legs
	numLegs := int(r.ReadInt())
	if numLegs > 0 {
		c.ComboLegs = make([]contract.ComboLeg, numLegs)
		for i := range numLegs {
			c.ComboLegs[i] = contract.ComboLeg{
				ConID:              r.ReadInt(),
				Ratio:              int(r.ReadInt()),
				Action:             r.ReadString(),
				Exchange:           r.ReadString(),
				OpenClose:          int(r.ReadInt()),
				ShortSaleSlot:      int(r.ReadInt()),
				DesignatedLocation: r.ReadString(),
				ExemptCode:         int(r.ReadInt()),
			}
		}
	}

	numOrderLegs := int(r.ReadInt())
	if numOrderLegs > 0 {
		o.OrderComboLegs = make([]order.OrderComboLeg, numOrderLegs)
		for i := range numOrderLegs {
			o.OrderComboLegs[i] = order.OrderComboLeg{Price: r.ReadFloat()}
		}
	}

	numParams := int(r.ReadInt())
	if numParams > 0 {
		o.SmartComboRoutingParams = make([]contract.TagValue, numParams)
		for i := range numParams {
			o.SmartComboRoutingParams[i] = contract.TagValue{Tag: r.ReadString(), Value: r.ReadString()}
		}
	}

	o.ScaleInitLevelSize = r.ReadInt()
	o.ScaleSubsLevelSize = r.ReadInt()
	scalePriceIncrement := r.ReadFloat()
	if scalePriceIncrement == 0 {
		scalePriceIncrement = UnsetDouble
	}
	o.ScalePriceIncrement = scalePriceIncrement
	if o.ScalePriceIncrement > 0 && o.ScalePriceIncrement < UnsetDouble {
		o.ScalePriceAdjustValue = r.ReadFloat()
		o.ScalePriceAdjustInterval = r.ReadInt()
		o.ScaleProfitOffset = r.ReadFloat()
		o.ScaleAutoReset = r.ReadBool()
		o.ScaleInitPosition = r.ReadInt()
		o.ScaleInitFillQty = r.ReadInt()
		o.ScaleRandomPercent = r.ReadBool()
	}

	o.HedgeType = r.ReadString()
	if o.HedgeType != "" {
		o.HedgeParam = r.ReadString()
	}

	o.ClearingAccount = r.ReadString()
	o.ClearingIntent = r.ReadString()
	o.NotHeld = r.ReadBool()

	dncPresent := r.ReadBool()
	if dncPresent {
		c.DeltaNeutralContract = &contract.DeltaNeutralContract{
			ConID: r.ReadInt(), Delta: r.ReadFloat(), Price: r.ReadFloat(),
		}
	}

	o.AlgoStrategy = r.ReadString()
	if o.AlgoStrategy != "" {
		numAlgoParams := int(r.ReadInt())
		if numAlgoParams > 0 {
			o.AlgoParams = make([]contract.TagValue, numAlgoParams)
			for i := range numAlgoParams {
				o.AlgoParams[i] = contract.TagValue{Tag: r.ReadString(), Value: r.ReadString()}
			}
		}
	}

	o.Solicited = r.ReadBool()
	st.Status = r.ReadString()
	o.RandomizeSize = r.ReadBool()
	o.RandomizePrice = r.ReadBool()

	if o.OrderType == "PEG BENCH" || o.OrderType == "PEGBENCH" {
		o.ReferenceContractID = r.ReadInt()
		o.IsPeggedChangeAmountDecrease = r.ReadBool()
		o.PeggedChangeAmount = r.ReadFloat()
		o.ReferenceChangeAmount = r.ReadFloat()
		o.ReferenceExchangeID = r.ReadString()
	}

	numConditions := int(r.ReadInt())
	if numConditions > 0 {
		o.Conditions = make([]order.OrderCondition, 0, numConditions)
		for range numConditions {
			condType := int(r.ReadInt())
			cond := order.CreateCondition(condType)
			if cond != nil {
				ib.readConditionFields(cond, r)
				o.Conditions = append(o.Conditions, cond)
			}
		}
		o.ConditionsIgnoreRTH = r.ReadBool()
		o.ConditionsCancelOrder = r.ReadBool()
	}

	o.TrailStopPrice = r.ReadFloat()
	o.LmtPriceOffset = r.ReadFloat()
	o.CashQty = r.ReadFloat()
	o.DontUseAutoPriceForHedge = r.ReadBool()
	o.IsOmsContainer = r.ReadBool()
	o.AutoCancelDate = r.ReadString()
	o.FilledQuantity = r.ReadFloat()
	o.RefFuturesConID = r.ReadInt()
	o.AutoCancelParent = r.ReadBool()
	o.Shareholder = r.ReadString()
	o.ImbalanceOnly = r.ReadBool()
	o.RouteMarketableToBbo = r.ReadBool()
	o.ParentPermID = r.ReadInt()
	st.CompletedTime = r.ReadString()
	st.CompletedStatus = r.ReadString()

	if ib.client.ServerVersion() >= 170 {
		o.MinTradeQty = r.ReadInt()
		o.MinCompeteSize = r.ReadInt()
		o.CompeteAgainstBestOffset = r.ReadFloat()
		o.MidOffsetAtWhole = r.ReadFloat()
		o.MidOffsetAtHalf = r.ReadFloat()
	}

	// Create trade and store
	os := &order.OrderStatus{OrderID: o.OrderID, Status: st.Status}
	trade := &order.Trade{Contract: c, Order: o, OrderStatus: os}

	ib.state.Mu.Lock()
	if o.PermID != 0 {
		if _, exists := ib.state.PermID2Trade[o.PermID]; !exists {
			ib.state.Trades[state.OrderKey{ClientID: 0, OrderID: o.PermID}] = trade
			ib.state.PermID2Trade[o.PermID] = trade
		}
	}
	ib.state.Mu.Unlock()

	if key := ib.completedReq.active(); key != 0 {
		ib.state.AppendResult(key, trade)
	}
}

func (ib *IB) handleCompletedOrdersEnd(r *protocol.FieldReader) {
	if key := ib.completedReq.active(); key != 0 {
		ib.state.EndReq(key, nil, nil)
	}
}

func (ib *IB) handleOrderBound(r *protocol.FieldReader) {
	// orderBound links an API order to its permId
	permID := r.ReadInt()
	apiClientID := r.ReadInt()
	apiOrderID := r.ReadInt()

	if permID != 0 && apiOrderID != 0 {
		key := state.OrderKey{ClientID: apiClientID, OrderID: apiOrderID}
		ib.state.Mu.Lock()
		trade, ok := ib.state.Trades[key]
		if ok {
			ib.state.PermID2Trade[permID] = trade
		}
		ib.state.Mu.Unlock()
		if ok {
			trade.SetPermID(permID)
		}
	}
}

// singleReq serialises a request type whose responses carry no request id.
// The slot is held for the whole request; handlers read the active key.
type singleReq struct {
	sem chan struct{}
	key atomic.Int64
	// roRejected is set while the in-flight request is one that TWS
	// rejects (with reqId -1) when the API is in read-only mode.
	roRejected atomic.Bool
}

func newSingleReq() *singleReq {
	return &singleReq{sem: make(chan struct{}, 1)}
}

// active returns the key of the in-flight request, or 0.
func (sr *singleReq) active() int64 {
	return sr.key.Load()
}

// readOnlyRejectedSingles returns the keys of in-flight id-less requests
// that TWS rejects in read-only mode.
func (ib *IB) readOnlyRejectedSingles() []int64 {
	var keys []int64
	for _, sr := range []*singleReq{ib.openOrdersReq, ib.completedReq} {
		if k := sr.active(); k != 0 && sr.roRejected.Load() {
			keys = append(keys, k)
		}
	}
	return keys
}

// runSingle runs one serialised request: waits for the slot, registers a
// synthetic key, sends, and waits for the end message.
func (ib *IB) runSingle(ctx context.Context, sr *singleReq, roRejected bool, send func() error) (interface{}, error) {
	select {
	case sr.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() {
		sr.key.Store(0)
		sr.roRejected.Store(false)
		<-sr.sem
	}()

	key := ib.syntheticReqID.Add(-1)
	ch := ib.state.StartReq(key, nil)
	sr.roRejected.Store(roRejected)
	sr.key.Store(key)
	if err := send(); err != nil {
		ib.state.CancelReq(key)
		return nil, err
	}
	return ib.awaitReq(ctx, key, ch)
}
