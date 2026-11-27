package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

// contractSpec is how agents describe an instrument.
type contractSpec struct {
	ConID           int64   `json:"con_id,omitempty" desc:"IB contract id. When set, the other fields are optional."`
	Symbol          string  `json:"symbol,omitempty" desc:"Ticker symbol, e.g. AAPL. For CASH use the pair, e.g. EURUSD."`
	SecType         string  `json:"sec_type,omitempty" enum:"STK,OPT,FUT,CONTFUT,FOP,CASH,IND,CFD,BOND,CMDTY,CRYPTO,FUND,WAR" desc:"Security type (default STK)."`
	Exchange        string  `json:"exchange,omitempty" desc:"Routing exchange (default SMART for STK/OPT, IDEALPRO for CASH)."`
	PrimaryExchange string  `json:"primary_exchange,omitempty" desc:"Listing exchange to disambiguate SMART-routed stocks, e.g. NASDAQ."`
	Currency        string  `json:"currency,omitempty" desc:"Currency (default USD for STK)."`
	Expiry          string  `json:"expiry,omitempty" desc:"Last trade date YYYYMMDD or contract month YYYYMM (OPT, FUT, FOP)."`
	Strike          float64 `json:"strike,omitempty" desc:"Option strike."`
	Right           string  `json:"right,omitempty" enum:"C,P" desc:"Option right: C (call) or P (put)."`
	Multiplier      string  `json:"multiplier,omitempty"`
	LocalSymbol     string  `json:"local_symbol,omitempty" desc:"Exchange-specific symbol, e.g. ESZ6 or an OCC option symbol."`
	TradingClass    string  `json:"trading_class,omitempty"`
}

func (c contractSpec) isEmpty() bool {
	return c.ConID == 0 && c.Symbol == "" && c.LocalSymbol == ""
}

// toContract builds an IB contract with defaults filled in.
func (c contractSpec) toContract() (*contract.Contract, error) {
	if c.isEmpty() {
		return nil, errors.New("contract needs con_id, symbol or local_symbol")
	}
	con := &contract.Contract{
		ConID:                        c.ConID,
		Symbol:                       strings.ToUpper(strings.TrimSpace(c.Symbol)),
		SecType:                      c.SecType,
		Exchange:                     strings.ToUpper(c.Exchange),
		PrimaryExchange:              strings.ToUpper(c.PrimaryExchange),
		Currency:                     strings.ToUpper(c.Currency),
		LastTradeDateOrContractMonth: c.Expiry,
		Strike:                       c.Strike,
		Right:                        c.Right,
		Multiplier:                   c.Multiplier,
		LocalSymbol:                  c.LocalSymbol,
		TradingClass:                 c.TradingClass,
	}
	if c.ConID != 0 {
		return con, nil
	}
	if con.SecType == "" {
		con.SecType = "STK"
	}
	switch con.SecType {
	case "STK", "OPT":
		if con.Exchange == "" {
			con.Exchange = "SMART"
		}
		if con.Currency == "" && con.SecType == "STK" {
			con.Currency = "USD"
		}
	case "CASH":
		if con.Exchange == "" {
			con.Exchange = "IDEALPRO"
		}
		if len(con.Symbol) == 6 && con.Currency == "" {
			con.Symbol, con.Currency = con.Symbol[:3], con.Symbol[3:]
		}
	}
	return con, nil
}

// resolve returns the details of the single contract matching spec.
func (s *Server) resolve(ctx context.Context, spec contractSpec) (contract.ContractDetails, error) {
	// The cache answers con_id-only lookups. Any other field (an explicit
	// exchange, for instance) goes to TWS, so it is not silently replaced
	// by the cached contract's.
	if spec.ConID != 0 && spec == (contractSpec{ConID: spec.ConID}) {
		s.detailsMu.Lock()
		d, ok := s.details[spec.ConID]
		s.detailsMu.Unlock()
		if ok {
			return d, nil
		}
	}
	con, err := spec.toContract()
	if err != nil {
		return contract.ContractDetails{}, err
	}
	if err := s.requireConnected(); err != nil {
		return contract.ContractDetails{}, err
	}
	matches, err := s.b.ReqContractDetails(ctx, con)
	if err != nil {
		return contract.ContractDetails{}, fmt.Errorf("contract lookup failed: %w", err)
	}
	switch len(matches) {
	case 0:
		return contract.ContractDetails{}, fmt.Errorf("no contract matches %s", describeSpec(con))
	case 1:
		d := matches[0]
		if d.Contract == nil || d.Contract.ConID == 0 {
			return contract.ContractDetails{}, errors.New("contract lookup returned no con_id")
		}
		s.cacheDetails(d)
		return d, nil
	}
	sortByExpiry(matches)
	var b strings.Builder
	fmt.Fprintf(&b, "%d contracts match %s; narrow it down (e.g. with con_id, primary_exchange, expiry, strike or right). Candidates:", len(matches), describeSpec(con))
	for i, m := range matches {
		if i == 10 {
			fmt.Fprintf(&b, " ... and %d more", len(matches)-10)
			break
		}
		if c := m.Contract; c != nil {
			cp := *c
			cp.ConID = 0 // already in front
			fmt.Fprintf(&b, "\n- con_id %d: %s", c.ConID, describeSpec(&cp))
		}
	}
	return contract.ContractDetails{}, errors.New(b.String())
}

// sortByExpiry orders contract matches by expiry, strike and right, so that
// the front month comes first and a limit keeps the nearest expirations
// (TWS returns futures in no particular order). Contracts without an expiry,
// such as stocks, keep TWS's order.
func sortByExpiry(matches []contract.ContractDetails) {
	key := func(d contract.ContractDetails) (string, float64, string) {
		if d.Contract == nil {
			return "\xff", 0, ""
		}
		return d.Contract.LastTradeDateOrContractMonth, d.Contract.Strike, d.Contract.Right
	}
	sort.SliceStable(matches, func(i, j int) bool {
		ei, si, ri := key(matches[i])
		ej, sj, rj := key(matches[j])
		if ei != ej {
			return ei < ej
		}
		if si != sj {
			return si < sj
		}
		return ri < rj
	})
}

func (s *Server) cacheDetails(d contract.ContractDetails) {
	s.detailsMu.Lock()
	defer s.detailsMu.Unlock()
	if len(s.details) >= 1000 {
		s.details = map[int64]contract.ContractDetails{}
	}
	s.details[d.Contract.ConID] = d
}

func describeSpec(c *contract.Contract) string {
	parts := []string{c.SecType}
	for _, p := range []string{c.Symbol, c.LocalSymbol, c.LastTradeDateOrContractMonth, c.Right, c.Exchange, c.PrimaryExchange, c.Currency} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if c.Strike != 0 {
		parts = append(parts, fmt.Sprintf("strike %g", c.Strike))
	}
	if c.ConID != 0 {
		parts = append(parts, fmt.Sprintf("con_id %d", c.ConID))
	}
	return strings.Join(parts, " ")
}

// --- get_status ---

type noArgs struct{}

type accountStatus struct {
	ID    string `json:"id"`
	Paper bool   `json:"paper"`
}

type tradingStatus struct {
	Enabled        bool        `json:"enabled"`
	AllowLive      bool        `json:"allow_live"`
	OrderTypes     []string    `json:"order_types,omitempty"`
	Halted         bool        `json:"halted,omitempty"`
	HaltReason     string      `json:"halt_reason,omitempty"`
	RealizedToday  *float64    `json:"realized_pnl_today,omitempty"`
	Limits         *limitsView `json:"limits,omitempty"`
	PreviewTTLSecs float64     `json:"preview_ttl_seconds,omitempty"`
	Blocked        string      `json:"blocked,omitempty"`
	NotReady       string      `json:"not_ready,omitempty"`
}

type limitsView struct {
	MaxOrderQuantity    float64  `json:"max_order_quantity,omitempty"`
	MaxOrderNotional    float64  `json:"max_order_notional,omitempty"`
	MaxPositionQuantity float64  `json:"max_position_quantity,omitempty"`
	MaxOpenOrders       int      `json:"max_open_orders,omitempty"`
	MaxOrdersPerWindow  int      `json:"max_orders_per_window,omitempty"`
	OrderWindow         string   `json:"order_window,omitempty"`
	PriceCollarPct      float64  `json:"price_collar_pct,omitempty"`
	DuplicateWindow     string   `json:"duplicate_window,omitempty"`
	MaxDailyLoss        float64  `json:"max_daily_loss,omitempty"`
	AllowedSecTypes     []string `json:"allowed_sec_types,omitempty"`
	AllowedSymbols      []string `json:"allowed_symbols,omitempty"`
	BlockedSymbols      []string `json:"blocked_symbols,omitempty"`
	AllowedAccounts     []string `json:"allowed_accounts,omitempty"`
}

func viewLimits(l risk.Limits) *limitsView {
	v := &limitsView{
		MaxOrderQuantity:    l.MaxOrderQuantity,
		MaxOrderNotional:    l.MaxOrderNotional,
		MaxPositionQuantity: l.MaxPositionQuantity,
		MaxOpenOrders:       l.MaxOpenOrders,
		MaxOrdersPerWindow:  l.MaxOrdersPerWindow,
		PriceCollarPct:      l.PriceCollarPct,
		MaxDailyLoss:        l.MaxDailyLoss,
		AllowedSecTypes:     l.AllowedSecTypes,
		AllowedSymbols:      l.AllowedSymbols,
		BlockedSymbols:      l.BlockedSymbols,
		AllowedAccounts:     l.AllowedAccounts,
	}
	if l.MaxOrdersPerWindow > 0 {
		v.OrderWindow = l.OrderWindow.String()
	}
	if l.DuplicateWindow > 0 {
		v.DuplicateWindow = l.DuplicateWindow.String()
	}
	return v
}

type statusResult struct {
	Connected      bool            `json:"connected"`
	ServerVersion  int             `json:"server_version,omitempty"`
	ClientID       int64           `json:"client_id,omitempty"`
	Accounts       []accountStatus `json:"accounts"`
	MarketDataType string          `json:"market_data_type"`
	// WhatIf tells whether preview_order asks TWS for margin and commission.
	WhatIf  bool          `json:"what_if"`
	Trading tradingStatus `json:"trading"`
}

func (s *Server) getStatus(_ context.Context, _ noArgs) (any, error) {
	res := statusResult{
		Connected:      s.b.IsConnected(),
		MarketDataType: marketDataTypes[s.cfg.MarketDataType],
		WhatIf:         !s.cfg.DisableWhatIf,
		Accounts:       []accountStatus{},
	}
	if res.Connected {
		res.ServerVersion = s.b.ServerVersion()
		res.ClientID = s.b.ClientID()
		for _, a := range s.b.ManagedAccounts() {
			res.Accounts = append(res.Accounts, accountStatus{ID: a, Paper: isPaperAccount(a)})
		}
	}
	tc := s.cfg.Trading
	res.Trading = tradingStatus{Enabled: tc.Enabled, AllowLive: tc.AllowLive}
	if tc.Enabled {
		res.Trading.OrderTypes = tc.OrderTypes
		res.Trading.PreviewTTLSecs = tc.PreviewTTL.Seconds()
		res.Trading.Halted, res.Trading.HaltReason = tc.Risk.Halted()
		pnl := tc.Risk.RealizedToday()
		res.Trading.RealizedToday = &pnl
		res.Trading.Limits = viewLimits(tc.Risk.Limits())
		if res.Connected && !tc.AllowLive {
			for _, a := range res.Accounts {
				if !a.Paper {
					res.Trading.Blocked = fmt.Sprintf("account %s is not a paper account and live trading is disabled", a.ID)
					break
				}
			}
		}
		if res.Connected && tc.Ready != nil {
			if err := tc.Ready(); err != nil {
				res.Trading.NotReady = "orders cannot be sent until the broker state is synchronised: " + err.Error()
			}
		}
	}
	return res, nil
}

// --- search_contracts ---

type searchArgs struct {
	Contract contractSpec `json:"contract" required:"true"`
	Limit    int          `json:"limit,omitempty" desc:"Maximum results (default 20, max 100)."`
	Detail   bool         `json:"detail,omitempty" desc:"Include order types, valid exchanges and trading hours."`
}

type searchResult struct {
	Count     int                   `json:"count"`
	Truncated bool                  `json:"truncated,omitempty"`
	Contracts []contractDetailsView `json:"contracts"`
}

func (s *Server) searchContracts(ctx context.Context, a searchArgs) (any, error) {
	con, err := a.Contract.toContract()
	if err != nil {
		return nil, err
	}
	if err := s.requireConnected(); err != nil {
		return nil, err
	}
	limit := clamp(a.Limit, 20, 100)
	matches, err := s.b.ReqContractDetails(ctx, con)
	if err != nil {
		return nil, fmt.Errorf("contract lookup failed: %w", err)
	}
	sortByExpiry(matches)
	res := searchResult{Count: len(matches), Contracts: []contractDetailsView{}}
	for i, m := range matches {
		if i == limit {
			res.Truncated = true
			break
		}
		if m.Contract != nil && m.Contract.ConID != 0 {
			s.cacheDetails(m)
		}
		res.Contracts = append(res.Contracts, viewDetails(m, a.Detail))
	}
	return res, nil
}

// --- get_quote ---

type quoteArgs struct {
	Contract contractSpec `json:"contract" required:"true"`
}

func (s *Server) getQuote(ctx context.Context, a quoteArgs) (any, error) {
	d, err := s.resolve(ctx, a.Contract)
	if err != nil {
		return nil, err
	}
	return s.quote(ctx, d.Contract)
}

func (s *Server) quote(ctx context.Context, con *contract.Contract) (quoteView, error) {
	if err := s.requireConnected(); err != nil {
		return quoteView{}, err
	}
	t, err := s.b.ReqTickerSnapshot(ctx, con)
	if err != nil {
		return quoteView{}, fmt.Errorf("market data request failed: %w", err)
	}
	s.rememberQuote(t)
	return viewQuote(t), nil
}

// --- get_historical_bars ---

type barsArgs struct {
	Contract    contractSpec `json:"contract" required:"true"`
	Duration    string       `json:"duration,omitempty" desc:"How far back, '<n> <S|D|W|M|Y>' (default '5 D')."`
	BarSize     string       `json:"bar_size,omitempty" enum:"1 secs,5 secs,10 secs,15 secs,30 secs,1 min,2 mins,3 mins,5 mins,10 mins,15 mins,20 mins,30 mins,1 hour,2 hours,3 hours,4 hours,8 hours,1 day,1 week,1 month" desc:"Bar size (default '1 day')."`
	WhatToShow  string       `json:"what_to_show,omitempty" enum:"TRADES,MIDPOINT,BID,ASK,BID_ASK,ADJUSTED_LAST,HISTORICAL_VOLATILITY,OPTION_IMPLIED_VOLATILITY" desc:"Data type (default TRADES; use MIDPOINT for CASH)."`
	EndDateTime string       `json:"end_date_time,omitempty" desc:"End of the period, 'YYYYMMDD HH:MM:SS' in exchange time or with a time zone, e.g. '20261120 16:00:00 US/Eastern'. Empty = now."`
	OutsideRTH  bool         `json:"outside_rth,omitempty" desc:"Include data outside regular trading hours."`
}

type barsResult struct {
	Contract  *contractView `json:"contract"`
	BarSize   string        `json:"bar_size"`
	Count     int           `json:"count"`
	Truncated bool          `json:"truncated,omitempty"`
	Bars      []barView     `json:"bars"`
}

var durationRE = regexp.MustCompile(`^[1-9][0-9]{0,4} [SDWMY]$`)

func (s *Server) getHistoricalBars(ctx context.Context, a barsArgs) (any, error) {
	if a.Duration == "" {
		a.Duration = "5 D"
	}
	a.Duration = strings.ToUpper(strings.TrimSpace(a.Duration))
	if !durationRE.MatchString(a.Duration) {
		return nil, fmt.Errorf("duration must look like '5 D' (units S, D, W, M, Y), got %q", a.Duration)
	}
	if a.BarSize == "" {
		a.BarSize = "1 day"
	}
	d, err := s.resolve(ctx, a.Contract)
	if err != nil {
		return nil, err
	}
	if a.WhatToShow == "" {
		a.WhatToShow = "TRADES"
		if d.Contract.SecType == "CASH" {
			a.WhatToShow = "MIDPOINT"
		}
	}
	bars, err := s.b.ReqHistoricalData(ctx, d.Contract, a.EndDateTime, a.Duration, a.BarSize, a.WhatToShow, !a.OutsideRTH, 1)
	if err != nil {
		return nil, fmt.Errorf("historical data request failed: %w", err)
	}
	res := barsResult{Contract: viewContract(d.Contract), BarSize: a.BarSize, Count: len(bars), Bars: []barView{}}
	if len(bars) > s.cfg.MaxBars {
		bars = bars[len(bars)-s.cfg.MaxBars:] // keep the most recent
		res.Truncated = true
	}
	daily := a.BarSize == "1 day" || a.BarSize == "1 week" || a.BarSize == "1 month"
	for _, b := range bars {
		res.Bars = append(res.Bars, viewBar(b, daily))
	}
	return res, nil
}

// --- get_option_chain ---

type chainArgs struct {
	Underlying     contractSpec `json:"underlying" required:"true" desc:"The underlying (e.g. a STK, IND or FUT contract)."`
	Exchange       string       `json:"exchange,omitempty" desc:"Only chains on this exchange (default SMART, or the future's exchange for a FUT underlying; '*' for all)."`
	MaxExpirations int          `json:"max_expirations,omitempty" desc:"Earliest expirations to return per chain (default 12, max 100)."`
	StrikeMin      float64      `json:"strike_min,omitempty"`
	StrikeMax      float64      `json:"strike_max,omitempty"`
}

type chainView struct {
	Exchange     string    `json:"exchange"`
	TradingClass string    `json:"trading_class"`
	Multiplier   string    `json:"multiplier"`
	Expirations  []string  `json:"expirations"`
	Strikes      []float64 `json:"strikes"`
	Truncated    bool      `json:"expirations_truncated,omitempty"`
}

type chainResult struct {
	Underlying *contractView `json:"underlying"`
	Chains     []chainView   `json:"chains"`
}

func (s *Server) getOptionChain(ctx context.Context, a chainArgs) (any, error) {
	d, err := s.resolve(ctx, a.Underlying)
	if err != nil {
		return nil, err
	}
	und := d.Contract
	futExchange := ""
	if und.SecType == "FUT" {
		futExchange = und.Exchange
	}
	chains, err := s.b.ReqSecDefOptParams(ctx, und.Symbol, futExchange, und.SecType, und.ConID)
	if err != nil {
		return nil, fmt.Errorf("option chain request failed: %w", err)
	}
	// Stock and index options are listed on SMART, futures options only on
	// the future's exchange (ES options: CME), so a FUT defaults to that.
	exch := strings.ToUpper(strings.TrimSpace(a.Exchange))
	defaulted := exch == ""
	if defaulted {
		exch = "SMART"
		if und.SecType == "FUT" && und.Exchange != "" {
			exch = und.Exchange
		}
	}
	maxExp := clamp(a.MaxExpirations, 12, 100)
	res := chainResult{Underlying: viewContract(und), Chains: []chainView{}}
	// When the default exchange lists none of the chains, return them all
	// rather than make the agent guess the exchange.
	all := exch == "*" || (defaulted && !anyChainOn(chains, exch))
	for _, c := range chains {
		if !all && c.Exchange != exch {
			continue
		}
		exps := append([]string(nil), c.Expirations...)
		sort.Strings(exps)
		v := chainView{Exchange: c.Exchange, TradingClass: c.TradingClass, Multiplier: c.Multiplier, Expirations: exps, Strikes: []float64{}}
		if len(exps) > maxExp {
			v.Expirations = exps[:maxExp]
			v.Truncated = true
		}
		strikes := append([]float64(nil), c.Strikes...)
		sort.Float64s(strikes)
		for _, k := range strikes {
			if (a.StrikeMin > 0 && k < a.StrikeMin) || (a.StrikeMax > 0 && k > a.StrikeMax) {
				continue
			}
			v.Strikes = append(v.Strikes, k)
		}
		res.Chains = append(res.Chains, v)
	}
	if len(res.Chains) == 0 && len(chains) > 0 {
		var exchanges []string
		for _, c := range chains {
			if !contains(exchanges, c.Exchange) {
				exchanges = append(exchanges, c.Exchange)
			}
		}
		return nil, fmt.Errorf("no option chain on exchange %s; available: %s", exch, strings.Join(exchanges, ", "))
	}
	return res, nil
}

func anyChainOn(chains []market.OptionChain, exch string) bool {
	for _, c := range chains {
		if c.Exchange == exch {
			return true
		}
	}
	return false
}

// --- get_positions ---

type positionsArgs struct {
	Account string `json:"account,omitempty" desc:"Only this account."`
}

type positionsResult struct {
	Positions []positionView `json:"positions"`
}

func (s *Server) getPositions(ctx context.Context, a positionsArgs) (any, error) {
	if err := s.requireConnected(); err != nil {
		return nil, err
	}
	pos, err := s.b.ReqPositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("positions request failed: %w", err)
	}
	sort.Slice(pos, func(i, j int) bool { return positionLess(pos[i], pos[j]) })
	res := positionsResult{Positions: []positionView{}}
	for _, p := range pos {
		if (a.Account != "" && p.Account != a.Account) || p.Position == 0 {
			continue
		}
		res.Positions = append(res.Positions, viewPosition(p))
	}
	return res, nil
}

func positionLess(a, b account.Position) bool {
	if a.Account != b.Account {
		return a.Account < b.Account
	}
	if a.Contract == nil || b.Contract == nil {
		return a.Contract == nil && b.Contract != nil
	}
	if a.Contract.Symbol != b.Contract.Symbol {
		return a.Contract.Symbol < b.Contract.Symbol
	}
	return a.Contract.ConID < b.Contract.ConID
}

// --- get_account_summary ---

type accountSummaryArgs struct {
	Account string   `json:"account,omitempty" desc:"Only this account."`
	Tags    []string `json:"tags,omitempty" enum:"AccountType,NetLiquidation,TotalCashValue,SettledCash,AccruedCash,BuyingPower,EquityWithLoanValue,PreviousEquityWithLoanValue,GrossPositionValue,RegTEquity,RegTMargin,SMA,InitMarginReq,MaintMarginReq,AvailableFunds,ExcessLiquidity,Cushion,FullInitMarginReq,FullMaintMarginReq,FullAvailableFunds,FullExcessLiquidity,LookAheadNextChange,LookAheadInitMarginReq,LookAheadMaintMarginReq,LookAheadAvailableFunds,LookAheadExcessLiquidity,HighestSeverity,DayTradesRemaining,Leverage" desc:"Tags to return (default: the main balance, margin and buying power values)."`
}

var defaultSummaryTags = []string{"NetLiquidation", "TotalCashValue", "BuyingPower", "AvailableFunds", "ExcessLiquidity", "InitMarginReq", "MaintMarginReq", "GrossPositionValue", "Cushion"}

type accountValueView struct {
	Account  string `json:"account"`
	Tag      string `json:"tag"`
	Value    string `json:"value"`
	Currency string `json:"currency,omitempty"`
}

type accountSummaryResult struct {
	Values []accountValueView `json:"values"`
}

func (s *Server) getAccountSummary(ctx context.Context, a accountSummaryArgs) (any, error) {
	if err := s.requireConnected(); err != nil {
		return nil, err
	}
	tags := a.Tags
	if len(tags) == 0 {
		tags = defaultSummaryTags
	}
	vals, err := s.b.ReqAccountSummary(ctx, "All", strings.Join(tags, ","))
	if err != nil {
		return nil, fmt.Errorf("account summary request failed: %w", err)
	}
	res := accountSummaryResult{Values: []accountValueView{}}
	for _, v := range vals {
		if a.Account != "" && v.Account != a.Account {
			continue
		}
		res.Values = append(res.Values, accountValueView{Account: v.Account, Tag: v.Tag, Value: v.Value, Currency: v.Currency})
	}
	sort.Slice(res.Values, func(i, j int) bool {
		if res.Values[i].Account != res.Values[j].Account {
			return res.Values[i].Account < res.Values[j].Account
		}
		return res.Values[i].Tag < res.Values[j].Tag
	})
	return res, nil
}

// --- list_orders / get_order ---

type listOrdersArgs struct {
	Scope string `json:"scope,omitempty" enum:"open,all" desc:"open (default): working orders of every API client and TWS; all: also the filled and cancelled orders this session has seen."`
}

type ordersResult struct {
	Orders []orderView `json:"orders"`
	Note   string      `json:"note,omitempty"`
}

func (s *Server) listOrders(ctx context.Context, a listOrdersArgs) (any, error) {
	me := s.b.ClientID()
	own := func(t *order.Trade) bool {
		o := t.OrderSnapshot()
		return me != 0 && o.ClientID == me && o.OrderID > 0
	}
	res := ordersResult{Orders: []orderView{}}

	// TWS tells this client about its own orders as they change, but about
	// other clients' and manual TWS orders only when asked (and sends no
	// updates for them afterwards). Ask every time, and leave out other
	// clients' orders that TWS no longer reports as open: what the session
	// knows about them is stale.
	others := map[*order.Trade]bool{}
	if err := s.requireConnected(); err != nil {
		res.Note = "not connected: only this client's orders known to the session are listed"
	} else if open, err := s.b.ReqAllOpenOrders(ctx); err != nil {
		res.Note = fmt.Sprintf("could not fetch the open orders of other API clients and TWS (%v); only this client's orders are listed", err)
	} else {
		for _, t := range open {
			if !own(t) && !t.IsDone() {
				others[t] = true
			}
		}
	}

	trades := s.b.OpenTrades()
	if a.Scope == "all" {
		trades = s.b.Trades()
	}
	for _, t := range trades {
		if !own(t) && !others[t] && !(a.Scope == "all" && t.IsDone()) {
			continue
		}
		res.Orders = append(res.Orders, viewTrade(t, me))
	}
	sort.Slice(res.Orders, func(i, j int) bool {
		if res.Orders[i].ClientID != res.Orders[j].ClientID {
			return res.Orders[i].ClientID < res.Orders[j].ClientID
		}
		if res.Orders[i].OrderID != res.Orders[j].OrderID {
			return res.Orders[i].OrderID < res.Orders[j].OrderID
		}
		return res.Orders[i].PermID < res.Orders[j].PermID
	})
	return res, nil
}

type getOrderArgs struct {
	OrderID  int64 `json:"order_id,omitempty" desc:"Order id (of this API client unless client_id is given)."`
	ClientID int64 `json:"client_id,omitempty" desc:"API client id that placed the order (default: this client)."`
	PermID   int64 `json:"perm_id,omitempty" desc:"Permanent order id; identifies orders of any client."`
}

func (s *Server) getOrder(_ context.Context, a getOrderArgs) (any, error) {
	t, err := s.findTrade(a.OrderID, a.ClientID, a.PermID)
	if err != nil {
		return nil, err
	}
	return withDetail(viewTrade(t, s.b.ClientID()), t, s.b.TradeFills(t)), nil
}

func (s *Server) findTrade(orderID, clientID, permID int64) (*order.Trade, error) {
	if orderID <= 0 && permID <= 0 {
		return nil, errors.New("give order_id or perm_id")
	}
	if clientID == 0 {
		clientID = s.b.ClientID()
	}
	for _, t := range s.b.Trades() {
		o := t.OrderSnapshot()
		if permID > 0 && (o.PermID == permID || t.PermID() == permID) {
			return t, nil
		}
		if permID <= 0 && o.OrderID == orderID && o.ClientID == clientID {
			return t, nil
		}
	}
	if permID > 0 {
		return nil, fmt.Errorf("no order with perm_id %d in this session (see list_orders)", permID)
	}
	return nil, fmt.Errorf("no order %d of client %d in this session (see list_orders)", orderID, clientID)
}

// --- get_executions ---

type executionsArgs struct {
	Symbol  string `json:"symbol,omitempty"`
	SecType string `json:"sec_type,omitempty" enum:"STK,OPT,FUT,FOP,CASH,CFD,BOND,CRYPTO,FUND,WAR,BAG"`
	Side    string `json:"side,omitempty" enum:"BUY,SELL"`
	Account string `json:"account,omitempty"`
}

type executionsResult struct {
	Executions []fillView `json:"executions"`
}

func (s *Server) getExecutions(ctx context.Context, a executionsArgs) (any, error) {
	if err := s.requireConnected(); err != nil {
		return nil, err
	}
	fills, err := s.b.ReqExecutions(ctx, order.ExecutionFilter{
		Symbol:   strings.ToUpper(a.Symbol),
		SecType:  a.SecType,
		Side:     a.Side,
		AcctCode: a.Account,
	})
	if err != nil {
		return nil, fmt.Errorf("executions request failed: %w", err)
	}
	sort.SliceStable(fills, func(i, j int) bool { return fillTime(fills[i]).Before(fillTime(fills[j])) })
	res := executionsResult{Executions: []fillView{}}
	for _, f := range fills {
		res.Executions = append(res.Executions, viewFill(f))
	}
	return res, nil
}

func fillTime(f order.Fill) time.Time {
	if f.Execution != nil && !f.Execution.Time.IsZero() {
		return f.Execution.Time
	}
	return f.Time
}

// clamp returns def for v <= 0 and caps v at max.
func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}
