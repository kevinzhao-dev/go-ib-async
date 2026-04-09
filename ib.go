// Package ibgo provides a Go client for the Interactive Brokers TWS/Gateway API.
package ibgo

import (
	"context"
	"fmt"
	"log"
	"strings"
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

const positionsReqID = int64(-1000) // dedicated key, avoids collision with IB's reqId=-1

// ReqPositions requests all positions.
func (ib *IB) ReqPositions(ctx context.Context) ([]account.Position, error) {
	reqID := positionsReqID
	ch := ib.state.StartReq(reqID, nil)

	if err := ib.client.ReqPositions(); err != nil {
		ib.state.Requests.Cancel(reqID)
		return nil, err
	}

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
		// no-op, openOrder messages already processed
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
	ib.state.EndReq(positionsReqID, nil, nil)
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
	// Simplified order status handling
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

	key := state.OrderKey{ClientID: clientID, OrderID: orderID}
	ib.state.Mu.Lock()
	trade, ok := ib.state.Trades[key]
	ib.state.Mu.Unlock()

	if ok {
		trade.OrderStatus = &order.OrderStatus{
			OrderID:      orderID,
			Status:       status,
			Filled:       filled,
			Remaining:    remaining,
			AvgFillPrice: avgFillPrice,
			PermID:       permID,
			ParentID:     parentID,
			LastFillPrice: lastFillPrice,
			ClientID:     clientID,
			WhyHeld:      whyHeld,
			MktCapPrice:  mktCapPrice,
		}
		ib.OrderStatusEvent.Emit(trade)
	}
}

func (ib *IB) handleOpenOrder(r *protocol.FieldReader) {
	// Simplified - just read orderId for now
	// Full openOrder parsing is the most complex handler (~200 lines)
	// and will be implemented incrementally
	_ = r
}

func (ib *IB) handleTickPrice(r *protocol.FieldReader) {
	r.Skip(1) // version
	reqID := r.ReadInt()
	tickType := int(r.ReadInt())
	price := r.ReadFloat()
	size := r.ReadFloat()

	ib.state.Mu.RLock()
	ticker, ok := ib.state.ReqID2Ticker[reqID]
	ib.state.Mu.RUnlock()

	if !ok {
		return
	}

	// Update price field
	if fieldName, ok := state.PriceTickMap[tickType]; ok {
		setTickerFloat(ticker, fieldName, price)
	}

	// Update corresponding size field
	if fieldName, ok := state.SizeTickMap[tickType]; ok {
		setTickerFloat(ticker, fieldName, size)
	}

	ticker.UpdateEvent.Emit(ticker)
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

	if fieldName, ok := state.SizeTickMap[tickType]; ok {
		setTickerFloat(ticker, fieldName, size)
	}

	ticker.UpdateEvent.Emit(ticker)
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

	ticker.UpdateEvent.Emit(ticker)
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

	if fieldName, ok := state.StringTickMap[tickType]; ok {
		setTickerString(ticker, fieldName, value)
	}
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
	_ = r.ReadString() // version
	reqID := r.ReadInt()
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
	_ = r.ReadString() // version
	reqID := r.ReadInt()

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

	trade := order.NewTrade(con, ord)

	key := state.OrderKey{ClientID: int64(ib.client.ClientID), OrderID: ord.OrderID}
	ib.state.Mu.Lock()
	ib.state.Trades[key] = trade
	ib.state.Mu.Unlock()

	if err := ib.client.Send(
		protocol.MsgPlaceOrder,
		ord.OrderID,
		con,
		con.SecIDType,
		con.SecID,
		ord.Action,
		ord.TotalQuantity,
		ord.OrderType,
		ord.LmtPrice,
		ord.AuxPrice,
		ord.TIF,
		ord.OcaGroup,
		ord.Account,
		ord.OpenClose,
		ord.Origin,
		ord.OrderRef,
		ord.Transmit,
		ord.ParentID,
		ord.BlockOrder,
		ord.SweepToFill,
		ord.DisplaySize,
		ord.TriggerMethod,
		ord.OutsideRTH,
		ord.Hidden,
	); err != nil {
		ib.state.Mu.Lock()
		delete(ib.state.Trades, key)
		ib.state.Mu.Unlock()
		return nil, err
	}

	ib.NewOrderEvent.Emit(trade)
	return trade, nil
}

// CancelOrder cancels an order.
func (ib *IB) CancelOrder(ord *order.Order) error {
	return ib.client.CancelOrder(ord.OrderID, "")
}
