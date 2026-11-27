package mcpserver

import (
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

// JSON views of library types: snake_case, unset values omitted.

type contractView struct {
	ConID           int64    `json:"con_id,omitempty"`
	Symbol          string   `json:"symbol,omitempty"`
	SecType         string   `json:"sec_type,omitempty"`
	LocalSymbol     string   `json:"local_symbol,omitempty"`
	Exchange        string   `json:"exchange,omitempty"`
	PrimaryExchange string   `json:"primary_exchange,omitempty"`
	Currency        string   `json:"currency,omitempty"`
	Expiry          string   `json:"expiry,omitempty"`
	Strike          *float64 `json:"strike,omitempty"`
	Right           string   `json:"right,omitempty"`
	Multiplier      string   `json:"multiplier,omitempty"`
	TradingClass    string   `json:"trading_class,omitempty"`
}

func viewContract(c *contract.Contract) *contractView {
	if c == nil {
		return nil
	}
	v := &contractView{
		ConID:           c.ConID,
		Symbol:          c.Symbol,
		SecType:         c.SecType,
		LocalSymbol:     c.LocalSymbol,
		Exchange:        c.Exchange,
		PrimaryExchange: c.PrimaryExchange,
		Currency:        c.Currency,
		Expiry:          c.LastTradeDateOrContractMonth,
		Right:           c.Right,
		Multiplier:      c.Multiplier,
		TradingClass:    c.TradingClass,
	}
	if c.Strike != 0 {
		v.Strike = num(c.Strike)
	}
	return v
}

type contractDetailsView struct {
	*contractView
	LongName       string   `json:"long_name,omitempty"`
	MinTick        *float64 `json:"min_tick,omitempty"`
	MinSize        *float64 `json:"min_size,omitempty"`
	SizeIncrement  *float64 `json:"size_increment,omitempty"`
	OrderTypes     string   `json:"order_types,omitempty"`
	ValidExchanges string   `json:"valid_exchanges,omitempty"`
	Industry       string   `json:"industry,omitempty"`
	Category       string   `json:"category,omitempty"`
	StockType      string   `json:"stock_type,omitempty"`
	UnderConID     int64    `json:"under_con_id,omitempty"`
	TimeZone       string   `json:"time_zone,omitempty"`
	TradingHours   string   `json:"trading_hours,omitempty"`
	LiquidHours    string   `json:"liquid_hours,omitempty"`
}

func viewDetails(d contract.ContractDetails, full bool) contractDetailsView {
	v := contractDetailsView{
		contractView: viewContract(d.Contract),
		LongName:     d.LongName,
		MinTick:      positive(d.MinTick),
		StockType:    d.StockType,
		UnderConID:   d.UnderConID,
		TimeZone:     d.TimeZoneID,
	}
	if v.contractView == nil {
		v.contractView = &contractView{}
	}
	if full {
		v.MinSize = positive(d.MinSize)
		v.SizeIncrement = positive(d.SizeIncrement)
		v.OrderTypes = d.OrderTypes
		v.ValidExchanges = d.ValidExchanges
		v.Industry = d.Industry
		v.Category = d.Category
		v.TradingHours = d.TradingHours
		v.LiquidHours = d.LiquidHours
	}
	return v
}

func positive(v float64) *float64 {
	if p := num(v); p != nil && *p > 0 {
		return p
	}
	return nil
}

var marketDataTypes = map[int]string{1: "live", 2: "frozen", 3: "delayed", 4: "delayed_frozen"}

type quoteView struct {
	Contract       *contractView `json:"contract"`
	MarketDataType string        `json:"market_data_type"`
	Time           string        `json:"time,omitempty"`
	Bid            *float64      `json:"bid,omitempty"`
	BidSize        *float64      `json:"bid_size,omitempty"`
	Ask            *float64      `json:"ask,omitempty"`
	AskSize        *float64      `json:"ask_size,omitempty"`
	Last           *float64      `json:"last,omitempty"`
	LastSize       *float64      `json:"last_size,omitempty"`
	Open           *float64      `json:"open,omitempty"`
	High           *float64      `json:"high,omitempty"`
	Low            *float64      `json:"low,omitempty"`
	Close          *float64      `json:"close,omitempty"`
	Volume         *float64      `json:"volume,omitempty"`
	MarkPrice      *float64      `json:"mark_price,omitempty"`
	Halted         bool          `json:"halted,omitempty"`
	Note           string        `json:"note,omitempty"`
}

func viewQuote(t *market.Ticker) quoteView {
	v := quoteView{
		Contract:       viewContract(t.Contract),
		MarketDataType: marketDataTypes[t.MarketDataType],
		Time:           timeString(t.Time),
		Bid:            price(t.Bid),
		Ask:            price(t.Ask),
		Last:           price(t.Last),
		Open:           price(t.Open),
		High:           price(t.High),
		Low:            price(t.Low),
		Close:          price(t.Close),
		Volume:         num(t.Volume),
		MarkPrice:      price(t.MarkPrice),
		Halted:         num(t.Halted) != nil && t.Halted > 0,
	}
	// A size without its price (TWS sends 0 with an absent bid or ask) says
	// nothing.
	if v.Bid != nil {
		v.BidSize = num(t.BidSize)
	}
	if v.Ask != nil {
		v.AskSize = num(t.AskSize)
	}
	if v.Last != nil {
		v.LastSize = num(t.LastSize)
	}
	if v.MarketDataType == "" {
		v.MarketDataType = "unknown"
	}
	if v.Bid == nil && v.Ask == nil && v.Last == nil && v.Close == nil {
		v.Note = "no prices returned: the instrument may not be trading, or the account lacks a market data subscription (consider the delayed market data type)"
	}
	return v
}

type barView struct {
	// Time is the start of an intraday bar (RFC 3339). Daily and longer
	// bars carry the date IB labels them with (YYYY-MM-DD): the session's
	// trade date, which for a session opening the evening before (futures)
	// is the day it closes, not the day it starts.
	Time     string   `json:"t"`
	Open     float64  `json:"o"`
	High     float64  `json:"h"`
	Low      float64  `json:"l"`
	Close    float64  `json:"c"`
	Volume   *float64 `json:"v,omitempty"`
	Average  *float64 `json:"avg,omitempty"`
	BarCount int      `json:"n,omitempty"`
}

func viewBar(b market.BarData, daily bool) barView {
	v := barView{
		Time:  timeString(b.Date),
		Open:  b.Open,
		High:  b.High,
		Low:   b.Low,
		Close: b.Close,
	}
	if daily && !b.Date.IsZero() {
		v.Time = b.Date.Format(time.DateOnly)
	}
	if b.BarCount > 0 { // -1: not reported (MIDPOINT, CASH)
		v.BarCount = b.BarCount
	}
	if b.Volume >= 0 {
		v.Volume = num(b.Volume)
	}
	if b.Average > 0 {
		v.Average = num(b.Average)
	}
	return v
}

type positionView struct {
	Account  string        `json:"account"`
	Contract *contractView `json:"contract"`
	Position float64       `json:"position"`
	AvgCost  *float64      `json:"avg_cost,omitempty"`
}

func viewPosition(p account.Position) positionView {
	return positionView{Account: p.Account, Contract: viewContract(p.Contract), Position: p.Position, AvgCost: num(p.AvgCost)}
}

type orderView struct {
	OrderID      int64         `json:"order_id"`
	ClientID     int64         `json:"client_id"`
	PermID       int64         `json:"perm_id,omitempty"`
	ParentID     int64         `json:"parent_id,omitempty"`
	Own          bool          `json:"own"`
	Account      string        `json:"account,omitempty"`
	Contract     *contractView `json:"contract"`
	Action       string        `json:"action"`
	Quantity     float64       `json:"quantity"`
	OrderType    string        `json:"order_type"`
	LimitPrice   *float64      `json:"limit_price,omitempty"`
	StopPrice    *float64      `json:"stop_price,omitempty"`
	TIF          string        `json:"tif,omitempty"`
	OutsideRTH   bool          `json:"outside_rth,omitempty"`
	OrderRef     string        `json:"order_ref,omitempty"`
	Status       string        `json:"status"`
	Filled       float64       `json:"filled"`
	Remaining    float64       `json:"remaining"`
	AvgFillPrice *float64      `json:"avg_fill_price,omitempty"`
	WhyHeld      string        `json:"why_held,omitempty"`
	LastError    string        `json:"last_error,omitempty"`
	// Stale marks a status that may be out of date (get_order of another
	// client's order that TWS no longer reports, or could not be asked).
	Stale bool       `json:"stale,omitempty"`
	Note  string     `json:"note,omitempty"`
	Fills []fillView `json:"fills,omitempty"`
	Log   []logView  `json:"log,omitempty"`
}

type logView struct {
	Time      string `json:"time"`
	Status    string `json:"status,omitempty"`
	Message   string `json:"message,omitempty"`
	ErrorCode int    `json:"error_code,omitempty"`
}

// viewTrade summarises t; withDetail adds its fills and event log.
func viewTrade(t *order.Trade, myClientID int64) orderView {
	o := t.OrderSnapshot()
	st := t.StatusSnapshot()
	v := orderView{
		OrderID:    o.OrderID,
		ClientID:   o.ClientID,
		PermID:     o.PermID,
		ParentID:   o.ParentID,
		Own:        o.ClientID == myClientID && o.OrderID > 0 && myClientID != 0,
		Account:    o.Account,
		Contract:   viewContract(t.Contract),
		Action:     o.Action,
		Quantity:   o.TotalQuantity,
		OrderType:  o.OrderType,
		TIF:        o.TIF,
		OutsideRTH: o.OutsideRTH,
		OrderRef:   o.OrderRef,
		Status:     st.Status,
		WhyHeld:    st.WhyHeld,
	}
	if o.OrderType != "MKT" && o.OrderType != "STP" {
		v.LimitPrice = num(o.LmtPrice)
	}
	if o.OrderType == "STP" || o.OrderType == "STP LMT" {
		v.StopPrice = num(o.AuxPrice)
	}
	if v.PermID == 0 {
		v.PermID = st.PermID
	}
	v.Filled = st.Filled
	if v.Filled == 0 {
		v.Filled = t.FilledQty()
	}
	v.Remaining = o.TotalQuantity - v.Filled
	if v.Remaining < 0 || order.DoneStates[st.Status] {
		v.Remaining = 0
	}
	if st.AvgFillPrice > 0 {
		v.AvgFillPrice = num(st.AvgFillPrice)
	}
	logs := t.LogSnapshot()
	for i := len(logs) - 1; i >= 0; i-- {
		if logs[i].ErrorCode != 0 {
			v.LastError = logs[i].Message
			break
		}
	}
	return v
}

// withDetail adds the fills and the event log of t to v. fills must be
// copies taken under the client's lock (Broker.TradeFills): the reader sets
// a fill's commission report while it runs.
func withDetail(v orderView, t *order.Trade, fills []order.Fill) orderView {
	for _, f := range fills {
		v.Fills = append(v.Fills, viewFill(f))
	}
	for _, e := range t.LogSnapshot() {
		v.Log = append(v.Log, logView{Time: timeString(e.Time), Status: e.Status, Message: e.Message, ErrorCode: e.ErrorCode})
	}
	return v
}

type fillView struct {
	ExecID      string        `json:"exec_id"`
	Time        string        `json:"time"`
	OrderID     int64         `json:"order_id,omitempty"`
	ClientID    int64         `json:"client_id,omitempty"`
	PermID      int64         `json:"perm_id,omitempty"`
	Account     string        `json:"account,omitempty"`
	Contract    *contractView `json:"contract,omitempty"`
	Side        string        `json:"side"`
	Shares      float64       `json:"shares"`
	Price       float64       `json:"price"`
	CumQty      float64       `json:"cum_qty,omitempty"`
	AvgPrice    float64       `json:"avg_price,omitempty"`
	Exchange    string        `json:"exchange,omitempty"`
	Commission  *float64      `json:"commission,omitempty"`
	Currency    string        `json:"commission_currency,omitempty"`
	RealizedPNL *float64      `json:"realized_pnl,omitempty"`
}

func viewFill(f order.Fill) fillView {
	v := fillView{Time: timeString(f.Time), Contract: viewContract(f.Contract)}
	if e := f.Execution; e != nil {
		v.ExecID = e.ExecID
		if !e.Time.IsZero() {
			v.Time = timeString(e.Time)
		}
		v.OrderID = e.OrderID
		v.ClientID = e.ClientID
		v.PermID = e.PermID
		v.Account = e.AcctNumber
		v.Side = e.Side
		v.Shares = e.Shares
		v.Price = e.Price
		v.CumQty = e.CumQty
		v.AvgPrice = e.AvgPrice
		v.Exchange = e.Exchange
	}
	if c := f.CommissionReport; c != nil {
		v.Commission = num(c.Commission)
		v.Currency = c.Currency
		v.RealizedPNL = num(c.RealizedPNL)
	}
	return v
}

type whatIfView struct {
	InitMarginBefore     *float64 `json:"init_margin_before,omitempty"`
	InitMarginChange     *float64 `json:"init_margin_change,omitempty"`
	InitMarginAfter      *float64 `json:"init_margin_after,omitempty"`
	MaintMarginChange    *float64 `json:"maint_margin_change,omitempty"`
	MaintMarginAfter     *float64 `json:"maint_margin_after,omitempty"`
	EquityWithLoanBefore *float64 `json:"equity_with_loan_before,omitempty"`
	EquityWithLoanAfter  *float64 `json:"equity_with_loan_after,omitempty"`
	Commission           *float64 `json:"commission,omitempty"`
	MinCommission        *float64 `json:"min_commission,omitempty"`
	MaxCommission        *float64 `json:"max_commission,omitempty"`
	CommissionCurrency   string   `json:"commission_currency,omitempty"`
	Warning              string   `json:"warning,omitempty"`
}

func viewWhatIf(st *order.OrderState) *whatIfView {
	if st == nil {
		return nil
	}
	parse := func(s string) *float64 {
		if v, ok := order.ParseValue(s); ok {
			return &v
		}
		return nil
	}
	return &whatIfView{
		InitMarginBefore:     parse(st.InitMarginBefore),
		InitMarginChange:     parse(st.InitMarginChange),
		InitMarginAfter:      parse(st.InitMarginAfter),
		MaintMarginChange:    parse(st.MaintMarginChange),
		MaintMarginAfter:     parse(st.MaintMarginAfter),
		EquityWithLoanBefore: parse(st.EquityWithLoanBefore),
		EquityWithLoanAfter:  parse(st.EquityWithLoanAfter),
		Commission:           num(st.Commission),
		MinCommission:        num(st.MinCommission),
		MaxCommission:        num(st.MaxCommission),
		CommissionCurrency:   st.CommissionCurrency,
		Warning:              st.WarningText,
	}
}
