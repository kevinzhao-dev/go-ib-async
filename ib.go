// Package ibgo provides a Go client for the Interactive Brokers TWS/Gateway API.
package ibgo

import (
	"context"
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

	// Serialization for subscription-style requests (IB sends one response stream)
	positionsMu       sync.Mutex
	positionsReqID    int64 // active key, protected by positionsMu
	completedOrdersMu sync.Mutex
	completedReqID    int64 // active key, protected by completedOrdersMu

	// Events
	ConnectedEvent    event.Event[struct{}]
	DisconnectedEvent event.Event[struct{}]
	ErrorEvent        event.Event[*IBError]
	UpdateEvent       event.Event[struct{}]

	// Order events
	NewOrderEvent     event.Event[*order.Trade]
	OrderStatusEvent  event.Event[*order.Trade]
	ExecDetailsEvent  event.Event[*order.Fill]

	// Market data events
	PendingTickersEvent event.Event[[]*market.Ticker]

	// Account events
	PositionEvent      event.Event[*account.Position]
	AccountValueEvent  event.Event[*account.AccountValue]
	PnLEvent           event.Event[*account.PnL]
}

// IBError wraps an IB API error with request context.
type IBError struct {
	ReqID    int64
	Code     int
	Message  string
	Contract *contract.Contract
}

func (e *IBError) Error() string {
	return fmt.Sprintf("ibgo: error %d (reqId %d): %s", e.Code, e.ReqID, e.Message)
}

// New creates a new IB client.
func New() *IB {
	ib := &IB{
		client: protocol.NewClient(),
		state:  state.NewManager(),
	}
	ib.client.OnMessage = ib.handleMessage
	ib.client.OnDisconnect = func(err error) {
		ib.state.Requests.DrainAll(ErrDisconnected)
		ib.DisconnectedEvent.Emit(struct{}{})
	}
	ib.client.OnDataArrived = func() {
		// Set timestamp for this message batch
		now := time.Now()
		ib.state.LastTime = now
		ib.state.TimeFloat = float64(now.UnixMilli()) / 1000.0
		// NOTE: We do NOT clear pending tickers here. Tickers accumulate
		// across messages in the same read cycle. They are cleared in
		// OnDataProcessed after events are emitted.
	}
	ib.client.OnDataProcessed = func() {
		ib.UpdateEvent.Emit(struct{}{})
		pending := ib.state.GetPendingTickers()
		if len(pending) > 0 {
			t := ib.state.LastTime
			ts := ib.state.TimeFloat
			for _, ticker := range pending {
				ticker.Time = t
				ticker.Timestamp = ts
				ticker.UpdateEvent.Emit(ticker)
			}
			ib.PendingTickersEvent.Emit(pending)
		}
		// Clear pending tickers and their tick lists AFTER emitting events
		ib.state.ClearPendingTickers()
	}
	return ib
}

// Connect establishes a connection to TWS/Gateway.
func (ib *IB) Connect(ctx context.Context, host string, port, clientID int) error {
	err := ib.client.Connect(ctx, host, port, clientID)
	if err != nil {
		return err
	}
	ib.ConnectedEvent.Emit(struct{}{})
	return nil
}

// Disconnect closes the connection.
func (ib *IB) Disconnect() {
	ib.client.Disconnect()
	ib.state.Reset()
}

// IsConnected returns true if connected to TWS/Gateway.
func (ib *IB) IsConnected() bool {
	return ib.client.IsConnected()
}

// ServerVersion returns the TWS/Gateway server version.
func (ib *IB) ServerVersion() int {
	return ib.client.ServerVersion
}

// ManagedAccounts returns the list of managed account IDs.
func (ib *IB) ManagedAccounts() []string {
	return ib.client.Accounts()
}

// --- Request-Response Methods ---

// ReqContractDetails requests full contract details.
func (ib *IB) ReqContractDetails(ctx context.Context, con *contract.Contract) ([]contract.ContractDetails, error) {
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, con)

	if err := ib.client.ReqContractDetails(reqID, con); err != nil {
		ib.state.Requests.Cancel(reqID)
		return nil, err
	}

	select {
	case result := <-ch:
		if result.Err != nil {
			return nil, result.Err
		}
		if result.Value == nil {
			return nil, nil
		}
		items := result.Value.([]interface{})
		details := make([]contract.ContractDetails, 0, len(items))
		for _, item := range items {
			if cd, ok := item.(*contract.ContractDetails); ok {
				details = append(details, *cd)
			}
		}
		return details, nil
	case <-ctx.Done():
		ib.state.Requests.Cancel(reqID)
		return nil, ctx.Err()
	case <-ib.client.Done():
		return nil, ErrDisconnected
	}
}

// ReqPositions requests all positions.
// Serialized: IB sends one positionEnd for all outstanding position requests.
func (ib *IB) ReqPositions(ctx context.Context) ([]account.Position, error) {
	ib.positionsMu.Lock()
	reqID := ib.syntheticReqID.Add(-1)
	ib.positionsReqID = reqID
	ch := ib.state.StartReq(reqID, nil)

	if err := ib.client.ReqPositions(); err != nil {
		ib.state.Requests.Cancel(reqID)
		ib.positionsMu.Unlock()
		return nil, err
	}
	ib.positionsMu.Unlock()

	select {
	case result := <-ch:
		if result.Err != nil {
			return nil, result.Err
		}
		return ib.state.GetPositions(), nil
	case <-ctx.Done():
		ib.state.Requests.Cancel(reqID)
		return nil, ctx.Err()
	case <-ib.client.Done():
		return nil, ErrDisconnected
	}
}

// ReqAccountSummary requests account summary values.
func (ib *IB) ReqAccountSummary(ctx context.Context, groupName, tags string) ([]account.AccountValue, error) {
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, nil)

	if err := ib.client.ReqAccountSummary(reqID, groupName, tags); err != nil {
		ib.state.Requests.Cancel(reqID)
		return nil, err
	}

	select {
	case result := <-ch:
		if result.Err != nil {
			return nil, result.Err
		}
		if result.Value == nil {
			return nil, nil
		}
		items := result.Value.([]interface{})
		values := make([]account.AccountValue, 0, len(items))
		for _, item := range items {
			if av, ok := item.(*account.AccountValue); ok {
				values = append(values, *av)
			}
		}
		return values, nil
	case <-ctx.Done():
		ib.state.Requests.Cancel(reqID)
		return nil, ctx.Err()
	case <-ib.client.Done():
		return nil, ErrDisconnected
	}
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
	default:
		// Unhandled message types are silently ignored for now
	}
}

func (ib *IB) handleError(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	code := int(r.ReadInt())
	msg := r.ReadString()

	// Warning/info codes (2100-2199) are not real errors
	if code >= 2100 && code < 2200 {
		log.Printf("ibgo: info %d: %s", code, msg)
		return
	}

	ibErr := &IBError{
		ReqID:   reqID,
		Code:    code,
		Message: msg,
	}

	// Look up associated contract
	ib.state.Mu.RLock()
	if con, ok := ib.state.ReqID2Contract[reqID]; ok {
		ibErr.Contract = con
	}
	ib.state.Mu.RUnlock()

	ib.ErrorEvent.Emit(ibErr)

	// If there's a pending request, complete it with the error
	if reqID >= 0 && ib.state.Requests.Has(reqID) {
		ib.state.EndReq(reqID, nil, &RequestError{ReqID: reqID, Code: code, Message: msg})
	}
}

func (ib *IB) handleContractDetails(r *protocol.FieldReader) {
	if ib.client.ServerVersion < 164 {
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

	if ib.client.ServerVersion < 164 {
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

	if ib.client.ServerVersion >= 164 {
		cd.MinSize = r.ReadFloat()
		cd.SizeIncrement = r.ReadFloat()
		cd.SuggestedSizeIncrement = r.ReadFloat()
	}

	// Parse lastTradeDateOrContractMonth from combined field
	if lastTimes != "" {
		sep := " "
		if len(lastTimes) > 8 && lastTimes[8] == '-' {
			sep = "-"
		}
		parts := splitN(lastTimes, sep)
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
		ConID:    r.ReadInt(),
		Symbol:   r.ReadString(),
		SecType:  r.ReadString(),
		LastTradeDateOrContractMonth: r.ReadString(),
		Strike:   r.ReadFloat(),
		Right:    r.ReadString(),
		Multiplier: r.ReadString(),
		Exchange: r.ReadString(),
		Currency: r.ReadString(),
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
	ib.positionsMu.Lock()
	key := ib.positionsReqID
	ib.positionsMu.Unlock()
	if key != 0 {
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

func (ib *IB) handleAccountDownloadEnd(r *protocol.FieldReader) {
	// Account download complete
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
	ib.state.Mu.Lock()
	key := orderKeyFromIDs(clientID, orderID, permID)
	trade, ok := ib.state.Trades[key]
	if !ok && permID != 0 {
		// Try permId lookup for manual/cross-client orders
		trade, ok = ib.state.PermID2Trade[permID], ib.state.PermID2Trade[permID] != nil
	}

	if ok {
		oldStatus := trade.OrderStatus.Status
		trade.OrderStatus = &order.OrderStatus{
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
		}
		ib.state.Mu.Unlock()

		// Log status change
		if status != oldStatus {
			logEntry := order.TradeLogEntry{
				Time:    time.Now(),
				Status:  status,
				Message: "",
			}
			trade.Log = append(trade.Log, logEntry)
		}

		ib.OrderStatusEvent.Emit(trade)
		trade.StatusEvent.Emit(trade)

		if status != oldStatus {
			if status == order.StatusFilled {
				trade.FilledEvent.Emit(trade)
			} else if status == order.StatusCancelled {
				trade.CancelledEvent.Emit(trade)
			}
		}
	} else {
		ib.state.Mu.Unlock()
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

	if ib.client.ServerVersion < 177 {
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

	if ib.client.ServerVersion >= 159 {
		o.Duration = r.ReadInt()
	}
	if ib.client.ServerVersion >= 160 {
		o.PostToAts = r.ReadInt()
	}
	if ib.client.ServerVersion >= 162 {
		o.AutoCancelParent = r.ReadBool()
	}
	if ib.client.ServerVersion >= 170 {
		o.MinTradeQty = r.ReadInt()
		o.MinCompeteSize = r.ReadInt()
		o.CompeteAgainstBestOffset = r.ReadFloat()
		o.MidOffsetAtWhole = r.ReadFloat()
		o.MidOffsetAtHalf = r.ReadFloat()
	}

	// Update trade state
	key := orderKeyFromOrder(o)
	ib.state.Mu.Lock()
	trade, exists := ib.state.Trades[key]
	if exists {
		// Update existing trade
		trade.Order.PermID = o.PermID
		trade.Order.TotalQuantity = o.TotalQuantity
		trade.Order.LmtPrice = o.LmtPrice
		trade.Order.AuxPrice = o.AuxPrice
		trade.Order.OrderType = o.OrderType
		trade.Order.OrderRef = o.OrderRef
	} else {
		// New trade from TWS or another client
		os := &order.OrderStatus{OrderID: o.OrderID, Status: st.Status}
		trade = &order.Trade{Contract: c, Order: o, OrderStatus: os}
		ib.state.Trades[key] = trade
	}
	if o.PermID != 0 {
		ib.state.PermID2Trade[o.PermID] = trade
	}
	ib.state.Mu.Unlock()

	ib.client.UpdateReqID(o.OrderID + 1)
	ib.OrderStatusEvent.Emit(trade)
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

func (ib *IB) handleTickPrice(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	price := r.ReadFloat()
	size := r.ReadFloat()
	_ = r.ReadString() // tickAttrib (ignored for now)

	ib.state.Mu.RLock()
	ticker, ok := ib.state.ReqID2Ticker[reqID]
	ib.state.Mu.RUnlock()

	if !ok {
		return
	}

	// Bid/Ask/Last are handled specially (tick types 1,2,4 + delayed 66,67,68)
	switch tickType {
	case 1, 66: // BID
		ticker.PrevBid = ticker.Bid
		ticker.PrevBidSize = ticker.BidSize
		ticker.Bid = price
		ticker.BidSize = size
	case 2, 67: // ASK
		ticker.PrevAsk = ticker.Ask
		ticker.PrevAskSize = ticker.AskSize
		ticker.Ask = price
		ticker.AskSize = size
	case 4, 68: // LAST
		ticker.PrevLast = ticker.Last
		ticker.PrevLastSize = ticker.LastSize
		ticker.Last = price
		ticker.LastSize = size
	default:
		if fieldName, ok := state.PriceTickMap[tickType]; ok {
			setTickerFloat(ticker, fieldName, price)
		}
	}

	ib.state.MarkTickerPending(ticker)
}

func (ib *IB) handleTickSize(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	size := r.ReadFloat()

	ib.state.Mu.RLock()
	ticker, ok := ib.state.ReqID2Ticker[reqID]
	ib.state.Mu.RUnlock()

	if !ok {
		return
	}

	// Bid/Ask/Last size are tick types 0,3,5 (and delayed 69,70,71)
	switch tickType {
	case 0, 69: // BID_SIZE
		ticker.PrevBidSize = ticker.BidSize
		ticker.BidSize = size
	case 3, 70: // ASK_SIZE
		ticker.PrevAskSize = ticker.AskSize
		ticker.AskSize = size
	case 5, 71: // LAST_SIZE
		ticker.PrevLastSize = ticker.LastSize
		ticker.LastSize = size
	default:
		if fieldName, ok := state.SizeTickMap[tickType]; ok {
			setTickerFloat(ticker, fieldName, size)
		}
	}

	ib.state.MarkTickerPending(ticker)
}

func (ib *IB) handleTickGeneric(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	value := r.ReadFloat()

	ib.state.Mu.RLock()
	ticker, ok := ib.state.ReqID2Ticker[reqID]
	ib.state.Mu.RUnlock()

	if !ok {
		return
	}

	if fieldName, ok := state.GenericTickMap[tickType]; ok {
		setTickerFloat(ticker, fieldName, value)
	}

	ib.state.MarkTickerPending(ticker)
}

func (ib *IB) handleTickString(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	value := r.ReadString()

	ib.state.Mu.RLock()
	ticker, ok := ib.state.ReqID2Ticker[reqID]
	ib.state.Mu.RUnlock()

	if !ok {
		return
	}

	switch {
	case tickType == 45 || tickType == 88:
		// Timestamp ticks (lastTimestamp, delayedLastTimestamp)
		ib.processTimestampTick(ticker, tickType, value)
	case tickType == 48 || tickType == 77:
		// RT Volume / RT Trade Volume
		ib.processRtVolumeTick(ticker, tickType, value)
	case tickType == 59:
		// Dividends: "past12,next12,nextDate,nextAmount"
		ib.processDividendTick(ticker, value)
	case tickType == 47:
		// Fundamental ratios: "key=value;key=value;..."
		ib.processFundamentalRatios(ticker, value)
	default:
		if fieldName, ok := state.StringTickMap[tickType]; ok {
			setTickerString(ticker, fieldName, value)
		}
	}

	ib.state.MarkTickerPending(ticker)
}

func (ib *IB) processTimestampTick(ticker *market.Ticker, tickType int, value string) {
	if value == "" || value == "0" {
		return
	}
	ts, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ts == 0 {
		return
	}
	t := time.Unix(ts, 0)

	switch tickType {
	case 45:
		ticker.LastTimestamp = t
	case 88:
		ticker.DelayedLastTimestamp = t
	}
}

func (ib *IB) processRtVolumeTick(ticker *market.Ticker, tickType int, value string) {
	// Format: price;size;ms_since_epoch;total_volume;VWAP;single_trade
	parts := strings.Split(value, ";")
	if len(parts) < 6 {
		return
	}

	priceStr, sizeStr, rtTimeStr, volumeStr, vwapStr := parts[0], parts[1], parts[2], parts[3], parts[4]

	if volumeStr != "" {
		if v, err := strconv.ParseFloat(volumeStr, 64); err == nil {
			switch tickType {
			case 48:
				ticker.RTVolume = v
			case 77:
				ticker.RTTradeVolume = v
			}
		}
	}

	if vwapStr != "" {
		if v, err := strconv.ParseFloat(vwapStr, 64); err == nil {
			ticker.VWAP = v
		}
	}

	if rtTimeStr != "" {
		if ms, err := strconv.ParseInt(rtTimeStr, 10, 64); err == nil {
			ticker.RTTime = time.UnixMilli(ms)
		}
	}

	if priceStr != "" {
		price, err1 := strconv.ParseFloat(priceStr, 64)
		size, err2 := strconv.ParseFloat(sizeStr, 64)
		if err1 == nil && err2 == nil {
			ticker.PrevLast = ticker.Last
			ticker.PrevLastSize = ticker.LastSize
			ticker.Last = price
			ticker.LastSize = size

			ticker.Ticks = append(ticker.Ticks, market.TickData{
				Time: ib.state.LastTime, TickType: tickType, Price: price, Size: size,
			})
		}
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

func splitN(s, sep string) []string {
	return strings.Split(s, sep)
}

// --- Historical Data ---

func (ib *IB) handleHistoricalData(r *protocol.FieldReader) {
	reqID := r.ReadInt() // no version field for historicalData
	_ = r.ReadString() // startDateStr
	_ = r.ReadString() // endDateStr
	numBars := int(r.ReadInt())

	bars := make([]market.BarData, 0, numBars)
	for range numBars {
		bar := market.BarData{
			Date:     parseBarDate(r.ReadString()),
			Open:     r.ReadFloat(),
			High:     r.ReadFloat(),
			Low:      r.ReadFloat(),
			Close:    r.ReadFloat(),
			Volume:   r.ReadFloat(),
			Average:  r.ReadFloat(),
			BarCount: int(r.ReadInt()),
		}
		bars = append(bars, bar)
	}

	ib.state.EndReq(reqID, bars, nil)
}

func (ib *IB) handleHistoricalDataUpdate(r *protocol.FieldReader) {
	reqID := r.ReadInt() // no version field

	bar := market.BarData{
		BarCount: int(r.ReadInt()),
		Date:     parseBarDate(r.ReadString()),
		Open:     r.ReadFloat(),
		Close:    r.ReadFloat(),
		High:     r.ReadFloat(),
		Low:      r.ReadFloat(),
		Average:  r.ReadFloat(),
		Volume:   r.ReadFloat(),
	}

	ib.state.Mu.RLock()
	sub, ok := ib.state.ReqID2Subscriber[reqID]
	ib.state.Mu.RUnlock()

	if ok {
		if bdl, ok := sub.(*market.BarDataList); ok {
			bdl.Bars = append(bdl.Bars, bar)
			bdl.UpdateEvent.Emit(bdl)
		}
	}
}

func parseBarDate(s string) time.Time {
	if len(s) == 8 {
		t, _ := time.Parse("20060102", s)
		return t
	}
	if len(s) == 10 {
		// Unix timestamp
		ts := int64(0)
		for _, c := range s {
			ts = ts*10 + int64(c-'0')
		}
		return time.Unix(ts, 0)
	}
	t, _ := time.Parse("20060102 15:04:05", s)
	return t
}

func (ib *IB) handleTickSnapshotEnd(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	ib.state.EndReq(reqID, nil, nil)
}

func (ib *IB) handleMarketDataType(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	mktDataType := int(r.ReadInt())

	ib.state.Mu.RLock()
	ticker, ok := ib.state.ReqID2Ticker[reqID]
	ib.state.Mu.RUnlock()

	if ok {
		ticker.MarketDataType = mktDataType
	}
}

// --- ReqHistoricalData public method ---

// ReqHistoricalData requests historical bar data.
func (ib *IB) ReqHistoricalData(ctx context.Context, con *contract.Contract, endDateTime, durationStr, barSizeSetting, whatToShow string, useRTH bool, formatDate int) ([]market.BarData, error) {
	reqID := ib.client.GetReqID()
	ch := ib.state.StartReq(reqID, con)

	if err := ib.client.ReqHistoricalData(reqID, con, endDateTime, durationStr, barSizeSetting, whatToShow, useRTH, formatDate, false, nil); err != nil {
		ib.state.Requests.Cancel(reqID)
		return nil, err
	}

	select {
	case result := <-ch:
		if result.Err != nil {
			return nil, result.Err
		}
		if bars, ok := result.Value.([]market.BarData); ok {
			return bars, nil
		}
		return nil, nil
	case <-ctx.Done():
		ib.state.Requests.Cancel(reqID)
		return nil, ctx.Err()
	case <-ib.client.Done():
		return nil, ErrDisconnected
	}
}

// --- ReqMktData subscription ---

// ReqMktData subscribes to streaming market data for a contract.
func (ib *IB) ReqMktData(con *contract.Contract, genericTickList string, snapshot, regulatorySnapshot bool) (*market.Ticker, error) {
	reqID := ib.client.GetReqID()
	ticker := market.NewTicker(con)

	ib.state.Mu.Lock()
	key := con.Key()
	ib.state.Tickers[key] = ticker
	ib.state.ReqID2Ticker[reqID] = ticker
	ib.state.Mu.Unlock()

	if err := ib.client.ReqMktData(reqID, con, genericTickList, snapshot, regulatorySnapshot, nil); err != nil {
		ib.state.Mu.Lock()
		delete(ib.state.Tickers, key)
		delete(ib.state.ReqID2Ticker, reqID)
		ib.state.Mu.Unlock()
		return nil, err
	}

	// For snapshot requests, set up request tracking
	if snapshot || regulatorySnapshot {
		ib.state.StartReq(reqID, con)
	}

	return ticker, nil
}

// CancelMktData unsubscribes from streaming market data.
func (ib *IB) CancelMktData(con *contract.Contract) {
	key := con.Key()

	ib.state.Mu.Lock()
	ticker, ok := ib.state.Tickers[key]
	if ok {
		delete(ib.state.Tickers, key)
		// Find and remove reqID mapping
		for reqID, t := range ib.state.ReqID2Ticker {
			if t == ticker {
				delete(ib.state.ReqID2Ticker, reqID)
				ib.client.CancelMktData(reqID)
				break
			}
		}
	}
	ib.state.Mu.Unlock()
}

// --- PlaceOrder ---

// PlaceOrder submits an order. Returns a Trade that tracks the order lifecycle.
func (ib *IB) PlaceOrder(con *contract.Contract, ord *order.Order) (*order.Trade, error) {
	if ord.OrderID == 0 {
		ord.OrderID = ib.client.GetReqID()
	}
	version := ib.client.ServerVersion

	// IB API BUG FIX: strip volatility for non-VOL orders
	if !strings.HasPrefix(ord.OrderType, "VOL") {
		ord.Volatility = UnsetDouble
	}

	now := time.Now()
	ord.ClientID = int64(ib.client.ClientID)
	key := orderKeyFromIDs(ord.ClientID, ord.OrderID, ord.PermID)

	ib.state.Mu.Lock()
	existingTrade, isModify := ib.state.Trades[key]
	ib.state.Mu.Unlock()

	var trade *order.Trade
	if isModify {
		// Modify existing order
		trade = existingTrade
		logEntry := order.TradeLogEntry{Time: now, Status: trade.OrderStatus.Status, Message: "Modify"}
		trade.Log = append(trade.Log, logEntry)
		trade.ModifyEvent.Emit(trade)
	} else {
		// New order
		os := &order.OrderStatus{OrderID: ord.OrderID, Status: order.StatusPendingSubmit}
		logEntry := order.TradeLogEntry{Time: now, Status: order.StatusPendingSubmit}
		trade = &order.Trade{
			Contract:    con,
			Order:       ord,
			OrderStatus: os,
			Log:         []order.TradeLogEntry{logEntry},
		}
		ib.state.Mu.Lock()
		ib.state.Trades[key] = trade
		ib.state.Mu.Unlock()
	}

	fields := []interface{}{
		protocol.MsgPlaceOrder,
		ord.OrderID,
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

	if err := ib.client.Send(fields...); err != nil {
		if !isModify {
			// Only remove from state if this was a new order;
			// modify failures must not lose the existing trade
			ib.state.Mu.Lock()
			delete(ib.state.Trades, key)
			ib.state.Mu.Unlock()
		}
		return nil, err
	}

	if !isModify {
		ib.NewOrderEvent.Emit(trade)
	}
	return trade, nil
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

// CancelOrder cancels an order.
func (ib *IB) CancelOrder(ord *order.Order) error {
	return ib.client.CancelOrder(ord.OrderID, "")
}

// --- Additional Handlers ---

func (ib *IB) handleOpenOrderEnd(r *protocol.FieldReader) {
	// Complete any pending reqOpenOrders
}

func (ib *IB) handleExecDetails(r *protocol.FieldReader) {
	// Python: _, reqId, ex.orderId, c.conId, c.symbol, c.secType, ...contract..., ex.execId, time, ...exec...
	// skip=2 default: msgId already skipped by Decoder, skip version here
	r.Skip(1) // version
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

	if ib.client.ServerVersion >= 178 {
		exec.PendingPriceRevision = r.ReadBool()
	}

	// IB TWS bug: manual order executions have orderId == UNSET_INTEGER
	if exec.OrderID == int64(math.MaxInt32) {
		exec.OrderID = 0
	}

	fill := &order.Fill{
		Contract:         c,
		Execution:        exec,
		CommissionReport: &order.CommissionReport{},
		Time:             exec.Time,
	}

	// Find the trade for this execution
	isLive := !ib.state.Requests.Has(reqID)
	ib.state.Mu.Lock()
	// First try permId, then orderKey
	trade := ib.state.PermID2Trade[exec.PermID]
	if trade == nil {
		key := orderKeyForExec(exec)
		trade = ib.state.Trades[key]
	}
	if _, exists := ib.state.Fills[exec.ExecID]; !exists {
		ib.state.Fills[exec.ExecID] = fill
		if trade != nil {
			trade.Fills = append(trade.Fills, fill)
			logEntry := order.TradeLogEntry{
				Time:    exec.Time,
				Status:  trade.OrderStatus.Status,
				Message: fmt.Sprintf("Fill %g@%g", exec.Shares, exec.Price),
			}
			trade.Log = append(trade.Log, logEntry)
		}
	}
	ib.state.Mu.Unlock()

	if isLive && trade != nil {
		ib.ExecDetailsEvent.Emit(fill)
		trade.FillEvent.Emit(fill)
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
		ExecID:             r.ReadString(),
		Commission:         r.ReadFloat(),
		Currency:           r.ReadString(),
		RealizedPNL:        r.ReadFloat(),
		Yield:              r.ReadFloat(),
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
	if ok {
		fill.CommissionReport = cr
	}
	// Find trade for commission event
	var trade *order.Trade
	if ok {
		trade = ib.state.PermID2Trade[fill.Execution.PermID]
	}
	ib.state.Mu.Unlock()

	if trade != nil && fill != nil {
		trade.CommissionReportEvent.Emit(fill)
		// FilledEvent is already emitted by handleOrderStatus on status transition;
		// do NOT re-emit here to avoid duplicate events
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
}

func (ib *IB) handleRealtimeBar(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	bar := market.RealTimeBar{
		Time:   time.Unix(r.ReadInt(), 0),
		Open:   r.ReadFloat(),
		High:   r.ReadFloat(),
		Low:    r.ReadFloat(),
		Close:  r.ReadFloat(),
		Volume: r.ReadFloat(),
		WAP:    r.ReadFloat(),
		Count:  int(r.ReadInt()),
	}

	ib.state.Mu.RLock()
	sub, ok := ib.state.ReqID2Subscriber[reqID]
	ib.state.Mu.RUnlock()

	if ok {
		if rtbl, ok := sub.(*market.RealTimeBarList); ok {
			rtbl.Bars = append(rtbl.Bars, bar)
			rtbl.UpdateEvent.Emit(rtbl)
		}
	}
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
		ib.state.Requests.Cancel(reqID)
		return nil, err
	}

	select {
	case result := <-ch:
		if result.Err != nil {
			return nil, result.Err
		}
		if result.Value == nil {
			return nil, nil
		}
		items := result.Value.([]interface{})
		chains := make([]market.OptionChain, 0, len(items))
		for _, item := range items {
			if c, ok := item.(*market.OptionChain); ok {
				chains = append(chains, *c)
			}
		}
		return chains, nil
	case <-ctx.Done():
		ib.state.Requests.Cancel(reqID)
		return nil, ctx.Err()
	case <-ib.client.Done():
		return nil, ErrDisconnected
	}
}

// ReqRealTimeBars subscribes to real-time 5-second bars.
func (ib *IB) ReqRealTimeBars(con *contract.Contract, barSize int, whatToShow string, useRTH bool) (*market.RealTimeBarList, error) {
	reqID := ib.client.GetReqID()
	rtbl := &market.RealTimeBarList{
		ReqID:    reqID,
		Contract: con,
		BarSize:  barSize,
		WhatToShow: whatToShow,
		UseRTH:   useRTH,
	}

	ib.state.Mu.Lock()
	ib.state.ReqID2Subscriber[reqID] = rtbl
	ib.state.Mu.Unlock()

	if err := ib.client.ReqRealTimeBars(reqID, con, barSize, whatToShow, useRTH, nil); err != nil {
		ib.state.Mu.Lock()
		delete(ib.state.ReqID2Subscriber, reqID)
		ib.state.Mu.Unlock()
		return nil, err
	}

	return rtbl, nil
}

// CancelRealTimeBars unsubscribes from real-time bars.
func (ib *IB) CancelRealTimeBars(rtbl *market.RealTimeBarList) {
	ib.state.Mu.Lock()
	delete(ib.state.ReqID2Subscriber, rtbl.ReqID)
	ib.state.Mu.Unlock()
	ib.client.CancelRealTimeBars(rtbl.ReqID)
}

// ReqCompletedOrders requests all completed (filled/cancelled) orders.
// Serialized: IB sends one completedOrdersEnd for all outstanding requests.
func (ib *IB) ReqCompletedOrders(ctx context.Context, apiOnly bool) ([]*order.Trade, error) {
	ib.completedOrdersMu.Lock()
	key := ib.syntheticReqID.Add(-1)
	ib.completedReqID = key

	ch := ib.state.StartReq(key, nil)
	if err := ib.client.ReqCompletedOrders(apiOnly); err != nil {
		ib.state.Requests.Cancel(key)
		ib.completedOrdersMu.Unlock()
		return nil, err
	}
	ib.completedOrdersMu.Unlock()
	select {
	case result := <-ch:
		if result.Err != nil {
			return nil, result.Err
		}
		if result.Value == nil {
			return nil, nil
		}
		items := result.Value.([]interface{})
		trades := make([]*order.Trade, 0, len(items))
		for _, item := range items {
			if t, ok := item.(*order.Trade); ok {
				trades = append(trades, t)
			}
		}
		return trades, nil
	case <-ctx.Done():
		ib.state.Requests.Cancel(key)
		return nil, ctx.Err()
	case <-ib.client.Done():
		return nil, ErrDisconnected
	}
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

	if ib.client.ServerVersion < 177 {
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

	if ib.client.ServerVersion >= 170 {
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

	ib.completedOrdersMu.Lock()
	key := ib.completedReqID
	ib.completedOrdersMu.Unlock()
	if key != 0 {
		ib.state.AppendResult(key, trade)
	}
}

func (ib *IB) handleCompletedOrdersEnd(r *protocol.FieldReader) {
	ib.completedOrdersMu.Lock()
	key := ib.completedReqID
	ib.completedOrdersMu.Unlock()
	if key != 0 {
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
		if trade, ok := ib.state.Trades[key]; ok {
			trade.Order.PermID = permID
			ib.state.PermID2Trade[permID] = trade
		}
		ib.state.Mu.Unlock()
	}
}

func (ib *IB) handleCurrentTime(r *protocol.FieldReader) {
	r.Skip(1) // version
	serverTime := r.ReadInt()
	ib.state.Mu.Lock()
	ib.state.LastTime = time.Unix(serverTime, 0)
	ib.state.Mu.Unlock()
}
