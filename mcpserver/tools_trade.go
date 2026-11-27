package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

// --- previews ---

const (
	kindNew     = "new"
	kindBracket = "bracket"
	kindModify  = "modify"

	maxPreviews = 50

	// quoteTimeout bounds the market data snapshot taken by preview_order
	// and place_order (TWS completes a snapshot after at most ~11s).
	quoteTimeout = 12 * time.Second
)

// bracketRoles names the legs of a bracket plan, in order.
var bracketRoles = []string{"entry", "take_profit", "stop_loss"}

// plan is a validated order waiting for place_order.
type plan struct {
	id      string
	kind    string
	con     *contract.Contract
	account string
	// orders holds the order (new, modify) or the bracket legs (parent,
	// take-profit, stop-loss). place_order sends copies.
	orders  []order.Order
	summary string
	created time.Time
	expires time.Time
}

type previewStore struct {
	mu    sync.Mutex
	plans map[string]*plan
}

func newPreviewStore() *previewStore {
	return &previewStore{plans: map[string]*plan{}}
}

func (ps *previewStore) put(p *plan) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := time.Now()
	for id, q := range ps.plans {
		if now.After(q.expires) {
			delete(ps.plans, id)
		}
	}
	for len(ps.plans) >= maxPreviews {
		var oldest *plan
		for _, q := range ps.plans {
			if oldest == nil || q.created.Before(oldest.created) {
				oldest = q
			}
		}
		delete(ps.plans, oldest.id)
	}
	ps.plans[p.id] = p
}

// take removes and returns a plan: a preview id can be placed only once.
func (ps *previewStore) take(id string) (*plan, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.plans[id]
	if p == nil {
		return nil, fmt.Errorf("unknown or already used preview_id %q; call preview_order again", id)
	}
	delete(ps.plans, id)
	if time.Now().After(p.expires) {
		return nil, fmt.Errorf("preview %s expired at %s; call preview_order again", id, timeString(p.expires))
	}
	return p, nil
}

func newPreviewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return "pv_" + hex.EncodeToString(b[:])
}

// --- preview_order ---

type previewArgs struct {
	Contract        contractSpec `json:"contract,omitempty" desc:"Instrument; prefer con_id. Not used with modify_order_id."`
	Action          string       `json:"action,omitempty" enum:"BUY,SELL" desc:"Side (required for new orders)."`
	Quantity        float64      `json:"quantity,omitempty" desc:"Number of shares/contracts (required for new orders)."`
	OrderType       string       `json:"order_type,omitempty" enum:"LMT,MKT,STP,STP LMT" desc:"Order type (default LMT). Allowed types are listed by get_status."`
	LimitPrice      float64      `json:"limit_price,omitempty" desc:"Limit price (LMT, STP LMT, bracket entry)."`
	StopPrice       float64      `json:"stop_price,omitempty" desc:"Stop trigger price (STP, STP LMT)."`
	TIF             string       `json:"tif,omitempty" enum:"DAY,GTC,IOC" desc:"Time in force (default DAY)."`
	OutsideRTH      bool         `json:"outside_rth,omitempty" desc:"Allow the order to work outside regular trading hours."`
	Account         string       `json:"account,omitempty" desc:"Account id; required when several accounts are managed."`
	TakeProfitPrice float64      `json:"take_profit_price,omitempty" desc:"Bracket: limit price of the profit-taking exit. Needs stop_loss_price and a LMT entry."`
	StopLossPrice   float64      `json:"stop_loss_price,omitempty" desc:"Bracket: stop price of the protective exit. Needs take_profit_price."`
	ModifyOrderID   int64        `json:"modify_order_id,omitempty" desc:"Modify this open order of this client instead of creating one. Give the new quantity, limit_price, stop_price and/or tif."`
}

type previewOrderView struct {
	Role       string   `json:"role"`
	Action     string   `json:"action"`
	Quantity   float64  `json:"quantity"`
	OrderType  string   `json:"order_type"`
	LimitPrice *float64 `json:"limit_price,omitempty"`
	StopPrice  *float64 `json:"stop_price,omitempty"`
	TIF        string   `json:"tif"`
	OutsideRTH bool     `json:"outside_rth,omitempty"`
}

type previewResult struct {
	PreviewID         string             `json:"preview_id,omitempty"`
	ExpiresAt         string             `json:"expires_at,omitempty"`
	Kind              string             `json:"kind"`
	Summary           string             `json:"summary"`
	Account           string             `json:"account"`
	PaperAccount      bool               `json:"paper_account"`
	Contract          *contractView      `json:"contract"`
	Orders            []previewOrderView `json:"orders"`
	ModifyOrderID     int64              `json:"modify_order_id,omitempty"`
	Quote             *quoteView         `json:"quote,omitempty"`
	EstimatedNotional *float64           `json:"estimated_notional,omitempty"`
	Currency          string             `json:"currency,omitempty"`
	WhatIf            *whatIfView        `json:"what_if,omitempty"`
	Warnings          []string           `json:"warnings,omitempty"`
	NextStep          string             `json:"next_step"`
}

func (s *Server) previewOrder(ctx context.Context, a previewArgs) (any, error) {
	if err := s.requireConnected(); err != nil {
		return nil, err
	}
	var (
		p   *plan
		err error
	)
	if a.ModifyOrderID != 0 {
		p, err = s.planModify(a)
	} else {
		p, err = s.planNew(ctx, a)
	}
	if err != nil {
		return nil, err
	}

	res := previewResult{
		Kind:          p.kind,
		Summary:       p.summary,
		Account:       p.account,
		PaperAccount:  isPaperAccount(p.account),
		Contract:      viewContract(p.con),
		ModifyOrderID: a.ModifyOrderID,
		Currency:      p.con.Currency,
	}
	roles := []string{"order"}
	if p.kind == kindBracket {
		roles = bracketRoles
	}
	for i, o := range p.orders {
		res.Orders = append(res.Orders, viewPreviewOrder(roles[i], o))
	}

	// Market context, best effort: also feeds the risk manager's reference
	// price (market-order notional, price collar).
	qctx, cancel := context.WithTimeout(ctx, quoteTimeout)
	q, qerr := s.quote(qctx, p.con)
	cancel()
	if qerr != nil {
		res.Warnings = append(res.Warnings, "no quote: "+qerr.Error())
	} else {
		res.Quote = &q
		if q.MarketDataType != "live" {
			res.Warnings = append(res.Warnings, "quote is "+q.MarketDataType+" market data, not live")
		}
	}

	entry := p.orders[0]
	if n, ok := s.notional(p.con, &entry); ok {
		res.EstimatedNotional = &n
	}
	res.Warnings = append(res.Warnings, s.policyWarnings(p)...)

	if p.kind == kindModify {
		res.Warnings = append(res.Warnings, "what-if is not available for modifications")
	} else {
		wi := entry
		if st, err := s.b.WhatIfOrder(ctx, p.con, &wi); err != nil {
			res.Warnings = append(res.Warnings, "what-if failed: "+err.Error())
		} else {
			res.WhatIf = viewWhatIf(st)
			if st.WarningText != "" {
				res.Warnings = append(res.Warnings, "IB: "+st.WarningText)
			}
		}
	}

	tc := s.cfg.Trading
	switch {
	case !tc.Enabled:
		res.NextStep = "Trading is disabled on this server; this preview is informational only."
	case !tc.AllowLive && !res.PaperAccount:
		res.NextStep = fmt.Sprintf("Account %s is not a paper account and live trading is disabled; this order cannot be placed.", p.account)
	default:
		p.created = time.Now()
		p.expires = p.created.Add(tc.PreviewTTL)
		p.id = newPreviewID()
		s.previews.put(p)
		res.PreviewID = p.id
		res.ExpiresAt = timeString(p.expires)
		res.NextStep = "Show this preview to the user. Only after the user explicitly confirms it, call place_order with this preview_id."
	}
	return res, nil
}

func viewPreviewOrder(role string, o order.Order) previewOrderView {
	v := previewOrderView{Role: role, Action: o.Action, Quantity: o.TotalQuantity, OrderType: o.OrderType, TIF: o.TIF, OutsideRTH: o.OutsideRTH}
	if o.OrderType == "LMT" || o.OrderType == "STP LMT" {
		v.LimitPrice = num(o.LmtPrice)
	}
	if o.OrderType == "STP" || o.OrderType == "STP LMT" {
		v.StopPrice = num(o.AuxPrice)
	}
	return v
}

func (s *Server) planNew(ctx context.Context, a previewArgs) (*plan, error) {
	if a.Action == "" {
		return nil, errors.New("missing required argument \"action\"")
	}
	if err := positiveFinite("quantity", a.Quantity, true); err != nil {
		return nil, err
	}
	if a.OrderType == "" {
		a.OrderType = "LMT"
	}
	if a.TIF == "" {
		a.TIF = "DAY"
	}
	if err := s.checkOrderType(a.OrderType); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		v    float64
	}{{"limit_price", a.LimitPrice}, {"stop_price", a.StopPrice}, {"take_profit_price", a.TakeProfitPrice}, {"stop_loss_price", a.StopLossPrice}} {
		if err := positiveFinite(f.name, f.v, false); err != nil {
			return nil, err
		}
	}
	if err := checkPrices(a.OrderType, a.LimitPrice, a.StopPrice); err != nil {
		return nil, err
	}
	bracket := a.TakeProfitPrice != 0 || a.StopLossPrice != 0
	if bracket && (a.TakeProfitPrice == 0 || a.StopLossPrice == 0) {
		return nil, errors.New("a bracket needs both take_profit_price and stop_loss_price")
	}
	if bracket && a.OrderType != "LMT" {
		return nil, errors.New("a bracket entry must be a LMT order")
	}
	if a.Contract.isEmpty() {
		return nil, errors.New("missing required argument \"contract\"")
	}

	d, err := s.resolve(ctx, a.Contract)
	if err != nil {
		return nil, err
	}
	acct, err := s.resolveAccount(a.Account)
	if err != nil {
		return nil, err
	}
	con := d.Contract
	p := &plan{con: con, account: acct}

	if bracket {
		b, err := order.NewBracket(a.Action, a.Quantity, a.LimitPrice, a.TakeProfitPrice, a.StopLossPrice)
		if err != nil {
			return nil, errors.New(strings.TrimPrefix(err.Error(), "order: "))
		}
		if err := s.checkOrderType("STP"); err != nil {
			return nil, fmt.Errorf("bracket stop-loss leg: %w", err)
		}
		p.kind = kindBracket
		for _, leg := range b.Orders() {
			s.decorate(leg, a, acct)
			p.orders = append(p.orders, *leg)
		}
		p.summary = fmt.Sprintf("%s %s: entry LMT %s, take-profit LMT %s, stop-loss STP %s, %s, account %s",
			a.Action, qtySymbol(a.Quantity, con), fmtPrice(a.LimitPrice), fmtPrice(a.TakeProfitPrice), fmtPrice(a.StopLossPrice), a.TIF, acct)
		return p, nil
	}

	o := order.NewOrder()
	o.Action = a.Action
	o.TotalQuantity = a.Quantity
	o.OrderType = a.OrderType
	if a.LimitPrice != 0 {
		o.LmtPrice = a.LimitPrice
	}
	if a.StopPrice != 0 {
		o.AuxPrice = a.StopPrice
	}
	s.decorate(o, a, acct)
	p.kind = kindNew
	p.orders = []order.Order{*o}
	p.summary = fmt.Sprintf("%s %s %s, %s, account %s", a.Action, qtySymbol(a.Quantity, con), describePrices(*o), a.TIF, acct)
	return p, nil
}

// decorate applies the order fields common to every new order.
func (s *Server) decorate(o *order.Order, a previewArgs, acct string) {
	o.TIF = a.TIF
	o.OutsideRTH = a.OutsideRTH
	o.Account = acct
	o.OrderRef = s.cfg.Trading.OrderRef
}

func (s *Server) planModify(a previewArgs) (*plan, error) {
	if a.TakeProfitPrice != 0 || a.StopLossPrice != 0 {
		return nil, errors.New("take_profit_price/stop_loss_price cannot be used with modify_order_id; modify each leg separately")
	}
	if _, err := s.ownClientID(); err != nil {
		return nil, err
	}
	t, err := s.findTrade(a.ModifyOrderID, 0, 0)
	if err != nil {
		return nil, err
	}
	if t.IsDone() {
		return nil, fmt.Errorf("order %d is already %s", a.ModifyOrderID, t.Status())
	}
	base := t.OrderSnapshot()
	if a.Action != "" && a.Action != base.Action {
		return nil, fmt.Errorf("cannot change the side of order %d (%s); cancel it and preview a new order", a.ModifyOrderID, base.Action)
	}
	if a.OrderType != "" && a.OrderType != base.OrderType {
		return nil, fmt.Errorf("cannot change the type of order %d (%s); cancel it and preview a new order", a.ModifyOrderID, base.OrderType)
	}
	if a.Contract.ConID != 0 && t.Contract != nil && a.Contract.ConID != t.Contract.ConID {
		return nil, fmt.Errorf("order %d is for con_id %d, not %d", a.ModifyOrderID, t.Contract.ConID, a.Contract.ConID)
	}
	if err := s.checkOrderType(base.OrderType); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		v    float64
	}{{"quantity", a.Quantity}, {"limit_price", a.LimitPrice}, {"stop_price", a.StopPrice}} {
		if err := positiveFinite(f.name, f.v, false); err != nil {
			return nil, err
		}
	}

	mod := base
	changed := false
	if a.Quantity != 0 && a.Quantity != base.TotalQuantity {
		if filled := t.FilledQty(); a.Quantity < filled {
			return nil, fmt.Errorf("quantity %g is below the %g already filled", a.Quantity, filled)
		}
		mod.TotalQuantity, changed = a.Quantity, true
	}
	if a.LimitPrice != 0 && a.LimitPrice != base.LmtPrice {
		if base.OrderType != "LMT" && base.OrderType != "STP LMT" {
			return nil, fmt.Errorf("order %d is %s and has no limit price", a.ModifyOrderID, base.OrderType)
		}
		mod.LmtPrice, changed = a.LimitPrice, true
	}
	if a.StopPrice != 0 && a.StopPrice != base.AuxPrice {
		if base.OrderType != "STP" && base.OrderType != "STP LMT" {
			return nil, fmt.Errorf("order %d is %s and has no stop price", a.ModifyOrderID, base.OrderType)
		}
		mod.AuxPrice, changed = a.StopPrice, true
	}
	if a.TIF != "" && a.TIF != base.TIF {
		mod.TIF, changed = a.TIF, true
	}
	if !changed {
		return nil, fmt.Errorf("nothing to change: give a new quantity, limit_price, stop_price or tif for order %d", a.ModifyOrderID)
	}
	mod.Transmit = true
	mod.WhatIf = false

	acct, err := s.resolveAccount(base.Account)
	if err != nil {
		return nil, err
	}
	con := t.Contract
	if con == nil {
		return nil, fmt.Errorf("order %d has no contract", a.ModifyOrderID)
	}
	return &plan{
		kind:    kindModify,
		con:     con,
		account: acct,
		orders:  []order.Order{mod},
		summary: fmt.Sprintf("MODIFY order %d: %s %s %s, %s (was %s %s)", a.ModifyOrderID, mod.Action, qtySymbol(mod.TotalQuantity, con),
			describePrices(mod), mod.TIF, fmtQty(base.TotalQuantity), describePrices(base)),
	}, nil
}

func (s *Server) checkOrderType(ot string) error {
	if !contains(s.cfg.Trading.OrderTypes, ot) {
		return fmt.Errorf("order type %s is not allowed on this server (allowed: %s)", ot, strings.Join(s.cfg.Trading.OrderTypes, ", "))
	}
	return nil
}

func checkPrices(orderType string, lmt, stp float64) error {
	switch orderType {
	case "LMT":
		if lmt == 0 {
			return errors.New("a LMT order needs limit_price")
		}
		if stp != 0 {
			return errors.New("stop_price is only used by STP and STP LMT orders")
		}
	case "MKT":
		if lmt != 0 || stp != 0 {
			return errors.New("a MKT order takes no limit_price or stop_price")
		}
	case "STP":
		if stp == 0 {
			return errors.New("a STP order needs stop_price")
		}
		if lmt != 0 {
			return errors.New("a STP order takes no limit_price; use STP LMT")
		}
	case "STP LMT":
		if lmt == 0 || stp == 0 {
			return errors.New("a STP LMT order needs limit_price and stop_price")
		}
	}
	return nil
}

func positiveFinite(name string, v float64, required bool) error {
	if v == 0 && !required {
		return nil
	}
	if !(v > 0) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a positive number, got %g", name, v)
	}
	return nil
}

// resolveAccount picks the order account: the given one (which must be
// managed), or the only managed account.
func (s *Server) resolveAccount(acct string) (string, error) {
	accounts := s.b.ManagedAccounts()
	if acct != "" {
		if !contains(accounts, acct) {
			return "", fmt.Errorf("account %q is not managed by this connection (accounts: %s)", acct, strings.Join(accounts, ", "))
		}
		return acct, nil
	}
	switch len(accounts) {
	case 0:
		return "", errors.New("the connection reports no managed accounts")
	case 1:
		return accounts[0], nil
	}
	return "", fmt.Errorf("several accounts are managed (%s); give account", strings.Join(accounts, ", "))
}

// notional estimates quantity * price * multiplier of an order, using its
// limit or stop price, else the reference quote.
func (s *Server) notional(con *contract.Contract, o *order.Order) (float64, bool) {
	px := 0.0
	switch {
	case o.LmtPrice != ibgo.UnsetDouble && o.LmtPrice > 0:
		px = o.LmtPrice
	case o.AuxPrice != ibgo.UnsetDouble && o.AuxPrice > 0:
		px = o.AuxPrice
	default:
		ref, ok := s.ReferencePrice(con)
		if !ok {
			return 0, false
		}
		px = ref
	}
	mult := 1.0
	if m, err := strconv.ParseFloat(con.Multiplier, 64); err == nil && m > 0 {
		mult = m
	}
	return math.Abs(o.TotalQuantity * px * mult), true
}

// policyWarnings flags what the risk manager will reject, so the agent
// learns it before asking the user. The risk manager stays the authority; it
// checks every leg of a bracket, and so does this.
func (s *Server) policyWarnings(p *plan) []string {
	tc := s.cfg.Trading
	if !tc.Enabled {
		return nil
	}
	var w []string
	if tc.Ready != nil {
		if err := tc.Ready(); err != nil {
			w = append(w, fmt.Sprintf("broker state is not synchronised yet (%v); place_order will be refused until it is", err))
		}
	}
	if halted, why := tc.Risk.Halted(); halted {
		w = append(w, "trading is halted ("+why+"); place_order will be rejected")
	}
	l := tc.Risk.Limits()
	if q := p.orders[0].TotalQuantity; l.MaxOrderQuantity > 0 && q > l.MaxOrderQuantity {
		w = append(w, fmt.Sprintf("quantity %g exceeds max_order_quantity %g; place_order will be rejected", q, l.MaxOrderQuantity))
	}
	ref, haveRef := s.ReferencePrice(p.con)
	collarUnchecked := false
	for i := range p.orders {
		o := &p.orders[i]
		leg := ""
		if p.kind == kindBracket {
			leg = bracketRoles[i] + " leg: "
		}
		if l.MaxOrderNotional > 0 {
			n, ok := s.notional(p.con, o)
			switch {
			case !ok:
				w = append(w, leg+"notional cannot be estimated (no price); place_order will be rejected by the notional limit")
			case n > l.MaxOrderNotional:
				w = append(w, fmt.Sprintf("%sestimated notional %.2f exceeds max_order_notional %.2f; place_order will be rejected", leg, n, l.MaxOrderNotional))
			}
		}
		if l.PriceCollarPct > 0 && hasLimitPrice(o) {
			if !haveRef {
				collarUnchecked = true
			} else if dev := math.Abs(o.LmtPrice-ref) / ref * 100; dev > l.PriceCollarPct {
				w = append(w, fmt.Sprintf("%slimit %s is %.2f%% from the reference price %s, more than price_collar_pct %g; place_order will be rejected",
					leg, fmtPrice(o.LmtPrice), dev, fmtPrice(ref), l.PriceCollarPct))
			}
		}
	}
	if collarUnchecked {
		w = append(w, "no reference price, so the price collar cannot check the limit price; place_order will be refused unless a quote is available then")
	}
	if len(l.AllowedSecTypes) > 0 && !contains(l.AllowedSecTypes, p.con.SecType) {
		w = append(w, fmt.Sprintf("sec_type %s is not in allowed_sec_types; place_order will be rejected", p.con.SecType))
	}
	sym := strings.ToUpper(p.con.Symbol)
	if contains(upperAll(l.BlockedSymbols), sym) || (len(l.AllowedSymbols) > 0 && !contains(upperAll(l.AllowedSymbols), sym)) {
		w = append(w, fmt.Sprintf("symbol %s is not allowed by the risk limits; place_order will be rejected", sym))
	}
	return w
}

// hasLimitPrice reports whether the risk manager's price collar applies to o.
func hasLimitPrice(o *order.Order) bool {
	return o.LmtPrice != ibgo.UnsetDouble && o.LmtPrice > 0 && !math.IsInf(o.LmtPrice, 0)
}

func upperAll(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func qtySymbol(qty float64, con *contract.Contract) string {
	name := con.Symbol
	if con.LocalSymbol != "" && con.SecType != "STK" {
		name = con.LocalSymbol
	}
	return fmt.Sprintf("%s %s (%s %s %s, con_id %d)", fmtQty(qty), name, con.SecType, con.Exchange, con.Currency, con.ConID)
}

func describePrices(o order.Order) string {
	switch o.OrderType {
	case "LMT":
		return "LMT " + fmtPrice(o.LmtPrice)
	case "STP":
		return "STP " + fmtPrice(o.AuxPrice)
	case "STP LMT":
		return "STP " + fmtPrice(o.AuxPrice) + " LMT " + fmtPrice(o.LmtPrice)
	}
	return o.OrderType
}

func fmtPrice(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
func fmtQty(v float64) string   { return strconv.FormatFloat(v, 'f', -1, 64) }

// --- place_order ---

type placeArgs struct {
	PreviewID   string `json:"preview_id" required:"true" desc:"preview_id from preview_order, after the user confirmed that preview."`
	WaitSeconds int    `json:"wait_seconds,omitempty" desc:"How long to wait for the broker to acknowledge (default 3, max 15)."`
}

type placeResult struct {
	Summary string      `json:"summary"`
	Orders  []orderView `json:"orders"`
	Note    string      `json:"note,omitempty"`
}

func (s *Server) placeOrder(ctx context.Context, a placeArgs) (any, error) {
	// Checked before the preview is used up, so the agent can retry it.
	if err := s.tradingReady(); err != nil {
		return nil, err
	}
	p, err := s.previews.take(a.PreviewID)
	if err != nil {
		return nil, err
	}
	if err := s.requireConnected(); err != nil {
		return nil, fmt.Errorf("%w; the preview was used up, preview the order again once connected", err)
	}
	if !s.cfg.Trading.AllowLive && !isPaperAccount(p.account) {
		return nil, fmt.Errorf("account %s is not a paper account and live trading is disabled", p.account)
	}
	if _, err := s.ownClientID(); err != nil {
		return nil, err
	}
	if err := s.refreshReference(ctx, p); err != nil {
		return nil, err
	}

	legs := make([]*order.Order, len(p.orders))
	for i := range p.orders {
		o := p.orders[i]
		legs[i] = &o
	}
	trades, err := s.submit(p, legs)
	if err != nil {
		var ns errNotSent
		if errors.As(err, &ns) {
			return nil, ns.error
		}
		if v, ok := risk.IsViolation(err); ok {
			return nil, fmt.Errorf("rejected by risk check %q: %s. Nothing was sent; report this to the user and do not work around it", v.Rule, v.Message)
		}
		if errors.Is(err, ibgo.ErrOrderDone) {
			return nil, fmt.Errorf("order is already done, nothing was sent: %w", err)
		}
		msg := fmt.Sprintf("order failed: %v", err)
		if len(trades) > 0 {
			msg += fmt.Sprintf(" (%d leg(s) had been sent and were cancelled; check list_orders)", len(trades))
		}
		return nil, errors.New(msg)
	}

	acked := s.waitAck(ctx, trades, p.kind == kindModify, waitDuration(a.WaitSeconds, 3, 15))
	me := s.b.ClientID()
	res := placeResult{Summary: p.summary, Orders: []orderView{}}
	for _, t := range trades {
		res.Orders = append(res.Orders, viewTrade(t, me))
	}
	if !acked {
		res.Note = "not yet acknowledged by the broker; check get_order"
	}
	for _, o := range res.Orders {
		if o.LastError != "" {
			res.Note = "the broker reported a problem: " + o.LastError
			break
		}
	}
	return res, nil
}

// errNotSent marks submit failures that happened before anything reached
// the client's PlaceOrder.
type errNotSent struct{ error }

// submit sends the plan's orders. It holds placeMu shared, so halt_trading
// (which takes it exclusively after halting) sees every order that passed the
// risk check before the halt. The hooks and readiness are checked again here,
// right before sending.
func (s *Server) submit(p *plan, legs []*order.Order) ([]*order.Trade, error) {
	s.placeMu.RLock()
	defer s.placeMu.RUnlock()
	if err := s.checkHooks(); err != nil {
		return nil, errNotSent{fmt.Errorf("order not sent: %w", err)}
	}
	if err := s.tradingReady(); err != nil {
		return nil, errNotSent{fmt.Errorf("%w (the preview was used up)", err)}
	}
	if p.kind == kindBracket {
		return s.b.PlaceBracketOrder(p.con, &order.BracketOrder{Parent: legs[0], TakeProfit: legs[1], StopLoss: legs[2]})
	}
	t, err := s.b.PlaceOrder(p.con, legs[0])
	if t == nil {
		return nil, err
	}
	return []*order.Trade{t}, err
}

// refreshReference makes sure the risk manager has a current reference
// price for the plan's contract. A preview can be placed for PreviewTTL,
// which may outlast the quote preview_order cached (QuoteMaxAge), and the
// risk manager skips the price collar when it has no reference. The quote is
// refreshed once it is older than half of QuoteMaxAge. With the collar
// enabled, an order whose limit price cannot be checked is refused.
func (s *Server) refreshReference(ctx context.Context, p *plan) error {
	if _, ok := s.referenceWithin(p.con, s.cfg.QuoteMaxAge/2); ok {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, quoteTimeout)
	_, qerr := s.quote(qctx, p.con)
	cancel()
	// Same margin as above: when the refresh failed, the old quote could
	// otherwise expire before the risk check runs, which then skips the
	// collar.
	if _, ok := s.referenceWithin(p.con, s.cfg.QuoteMaxAge/2); ok {
		return nil
	}
	collared := false
	for i := range p.orders {
		collared = collared || hasLimitPrice(&p.orders[i])
	}
	if s.cfg.Trading.Risk.Limits().PriceCollarPct <= 0 || !collared {
		// Market orders without a reference fail the notional check.
		return nil
	}
	why := "the quote returned no prices"
	if qerr != nil {
		why = qerr.Error()
	}
	return fmt.Errorf("no current reference price for %s (%s), so the price collar cannot check the limit price. Nothing was sent and the preview was used up; preview the order again once quotes are available",
		describeSpec(p.con), why)
}

func waitDuration(secs, def, max int) time.Duration {
	if secs < 0 {
		secs = 0
	} else if secs == 0 {
		secs = def
	}
	if secs > max {
		secs = max
	}
	return time.Duration(secs) * time.Second
}

// waitAck waits until the broker acknowledged every trade (or d elapsed):
// a new order leaves PendingSubmit, a modification gets its "Modified" (or
// an error) log entry.
func (s *Server) waitAck(ctx context.Context, trades []*order.Trade, modify bool, d time.Duration) bool {
	return poll(ctx, d, func() bool {
		for _, t := range trades {
			if t.IsDone() {
				continue
			}
			if modify {
				logs := t.LogSnapshot()
				if len(logs) > 0 && logs[len(logs)-1].Message == "Modify" {
					return false
				}
				continue
			}
			switch t.Status() {
			case "", order.StatusPendingSubmit, order.StatusApiPending:
				return false
			}
		}
		return true
	})
}

// poll calls cond every 20ms until it returns true (true), or d elapses or
// ctx ends (false).
func poll(ctx context.Context, d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// --- cancel_order / cancel_all_orders / halt_trading ---

type cancelArgs struct {
	OrderID     int64 `json:"order_id" required:"true" desc:"Order id of an open order of this client."`
	WaitSeconds int   `json:"wait_seconds,omitempty" desc:"How long to wait for the cancel confirmation (default 3, max 15)."`
}

type cancelResult struct {
	Orders []orderView `json:"orders"`
	// Skipped are orders of live accounts left alone because live trading
	// is disabled.
	Skipped []orderView `json:"skipped,omitempty"`
	Note    string      `json:"note,omitempty"`
}

func (s *Server) cancelOrder(ctx context.Context, a cancelArgs) (any, error) {
	if err := s.requireConnected(); err != nil {
		return nil, err
	}
	if _, err := s.ownClientID(); err != nil {
		return nil, err
	}
	t, err := s.findTrade(a.OrderID, 0, 0)
	if err != nil {
		return nil, err
	}
	if t.IsDone() {
		return nil, fmt.Errorf("order %d is already %s", a.OrderID, t.Status())
	}
	if acct := t.OrderSnapshot().Account; !s.cancelAllowed(acct) {
		return nil, fmt.Errorf("order %d is for account %s, which is not a paper account, and live trading is disabled on this server; it cannot be cancelled here", a.OrderID, acct)
	}
	return s.cancelTrades(ctx, []*order.Trade{t}, waitDuration(a.WaitSeconds, 3, 15))
}

// cancelAllowed applies the paper-only policy to cancellations too.
func (s *Server) cancelAllowed(account string) bool {
	return s.cfg.Trading.AllowLive || isPaperAccount(account)
}

type cancelAllArgs struct {
	WaitSeconds int `json:"wait_seconds,omitempty" desc:"How long to wait for the cancel confirmations (default 3, max 15)."`
}

func (s *Server) cancelAllOrders(ctx context.Context, a cancelAllArgs) (any, error) {
	if err := s.requireConnected(); err != nil {
		return nil, err
	}
	own, err := s.ownOpenTrades()
	if err != nil {
		return nil, err
	}
	return s.cancelTrades(ctx, own, waitDuration(a.WaitSeconds, 3, 15))
}

// ownOpenTrades returns the open orders of this client. Children of a
// bracket parent that is open and has no fills are left out: cancelling the
// parent cancels them. After a partial fill IB keeps the children working
// for the filled quantity, so they are included.
func (s *Server) ownOpenTrades() ([]*order.Trade, error) {
	me, err := s.ownClientID()
	if err != nil {
		return nil, err
	}
	var own []*order.Trade
	unfilledParent := map[int64]bool{}
	for _, t := range s.b.OpenTrades() {
		o := t.OrderSnapshot()
		if o.ClientID == me && o.OrderID > 0 && !t.IsDone() {
			own = append(own, t)
			unfilledParent[o.OrderID] = t.FilledQty() == 0 && t.StatusSnapshot().Filled == 0
		}
	}
	out := own[:0]
	for _, t := range own {
		if p := t.OrderSnapshot().ParentID; p != 0 && unfilledParent[p] {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// cancelTrades cancels trades, skipping those the paper-only policy
// protects, and waits up to wait for the confirmations.
func (s *Server) cancelTrades(ctx context.Context, trades []*order.Trade, wait time.Duration) (cancelResult, error) {
	me := s.b.ClientID()
	res := cancelResult{Orders: []orderView{}}
	var errs []string
	var sent []*order.Trade
	for _, t := range trades {
		o := t.OrderSnapshot()
		if !s.cancelAllowed(o.Account) {
			res.Skipped = append(res.Skipped, viewTrade(t, me))
			continue
		}
		ref := &order.Order{OrderID: o.OrderID, ClientID: o.ClientID, PermID: t.PermID()}
		if _, err := s.b.CancelOrder(ref); err != nil {
			errs = append(errs, fmt.Sprintf("order %d: %v", o.OrderID, err))
		}
		sent = append(sent, t)
	}
	poll(ctx, wait, func() bool {
		for _, t := range sent {
			if !t.IsDone() {
				return false
			}
		}
		return true
	})
	pending := 0
	for _, t := range sent {
		res.Orders = append(res.Orders, viewTrade(t, me))
		if !t.IsDone() {
			pending++
		}
	}
	var notes []string
	switch {
	case len(errs) > 0:
		notes = append(notes, "some cancels failed: "+strings.Join(errs, "; "))
	case len(trades) == 0:
		notes = append(notes, "no open orders of this client")
	case pending > 0:
		notes = append(notes, fmt.Sprintf("%d cancel(s) not yet confirmed; check get_order", pending))
	}
	if n := len(res.Skipped); n > 0 {
		notes = append(notes, fmt.Sprintf("%d order(s) of live accounts were not cancelled: live trading is disabled on this server", n))
	}
	res.Note = strings.Join(notes, "; ")
	return res, nil
}

type haltArgs struct {
	Reason           string `json:"reason" required:"true" desc:"Why trading is halted (shown to the user and logged)."`
	CancelOpenOrders bool   `json:"cancel_open_orders,omitempty" desc:"Also cancel all open orders of this client."`
}

type haltResult struct {
	Halted    bool          `json:"halted"`
	Reason    string        `json:"reason"`
	Cancelled *cancelResult `json:"cancelled,omitempty"`
	Note      string        `json:"note"`
}

func (s *Server) haltTrading(ctx context.Context, a haltArgs) (any, error) {
	reason := "halted by agent: " + a.Reason
	s.cfg.Trading.Risk.Halt(reason)
	res := haltResult{Halted: true, Reason: reason, Note: "New orders and modifications are rejected until a human restarts the server."}
	if a.CancelOpenOrders {
		if err := s.requireConnected(); err != nil {
			return nil, fmt.Errorf("trading is halted, but open orders could not be cancelled: %w", err)
		}
		// Wait for submissions that passed the risk check before the halt,
		// so their orders are registered and cancelled below. Later ones
		// are rejected by the halt.
		s.placeMu.Lock()
		own, err := s.ownOpenTrades()
		s.placeMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("trading is halted, but open orders could not be cancelled: %w", err)
		}
		c, _ := s.cancelTrades(ctx, own, 3*time.Second)
		res.Cancelled = &c
	}
	return res, nil
}
