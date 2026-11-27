// Package risk implements pre-trade risk checks for ibgo.
//
// A *Manager satisfies ibgo.PreTradeChecker (and ibgo.CommissionObserver,
// ibgo.BracketChecker):
//
//	rm := risk.New(risk.Limits{MaxOrderQuantity: 500, MaxPositionQuantity: 1000}, ib)
//	ib.SetRiskManager(rm)
//
// Every PlaceOrder/PlaceBracketOrder is checked before anything is sent; a
// rejected order returns a *Violation and never reaches the broker. Limits
// with a zero value are disabled. Checks fail closed: if a limit needs data
// that is unavailable (e.g. a price for the notional limit), the order is
// rejected.
package risk

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

// Rule names reported in a Violation.
const (
	RuleHalted        = "halted"
	RuleInvalidOrder  = "invalid_order"
	RuleSecType       = "sec_type"
	RuleSymbol        = "symbol"
	RuleAccount       = "account"
	RuleOrderQuantity = "order_quantity"
	RuleOrderNotional = "order_notional"
	RulePosition      = "position"
	RuleOpenOrders    = "open_orders"
	RuleOrderRate     = "order_rate"
	RulePriceCollar   = "price_collar"
	RuleDuplicate     = "duplicate"
	RuleDailyLoss     = "daily_loss"
)

// Violation is the error returned for a rejected order.
type Violation struct {
	Rule    string
	Message string
}

func (v *Violation) Error() string {
	return fmt.Sprintf("risk: %s: %s", v.Rule, v.Message)
}

// IsViolation reports whether err is (or wraps) a risk violation, and
// returns it.
func IsViolation(err error) (*Violation, bool) {
	var v *Violation
	ok := errors.As(err, &v)
	return v, ok
}

// Limits configures the checks. Zero values disable a check.
type Limits struct {
	// MaxOrderQuantity caps the quantity of a single order. A cash-sized
	// order with unknown share quantity cannot satisfy this limit.
	MaxOrderQuantity float64
	// MaxOrderNotional caps quantity * price * multiplier of a single order
	// (in the contract's currency). Needs a price: the limit price, the stop
	// price, or the PriceSource for market orders.
	MaxOrderNotional float64
	// MaxPositionQuantity caps the absolute position per contract (conId)
	// assuming every open order on it fills (worst case per side). Child
	// orders of a bracket (ParentID != 0) only close the parent's exposure
	// and are not counted.
	MaxPositionQuantity float64
	// MaxOpenOrders caps the number of active orders (new orders only).
	MaxOpenOrders int
	// MaxOrdersPerWindow caps order submissions (new + modify) per
	// OrderWindow (default window 1s).
	MaxOrdersPerWindow int
	OrderWindow        time.Duration
	// PriceCollarPct rejects limit prices further than this percentage from
	// the PriceSource reference (e.g. 5 = ±5%). Skipped when no reference
	// price is available.
	PriceCollarPct float64
	// DuplicateWindow rejects a new order identical (contract, side,
	// quantity, type, prices) to one submitted within this window.
	DuplicateWindow time.Duration
	// MaxDailyLoss halts trading once today's realized loss (from commission
	// reports, net of commissions) reaches this amount (positive number).
	MaxDailyLoss float64
	// AllowedSecTypes / AllowedSymbols restrict what may be traded (empty =
	// anything). BlockedSymbols always wins.
	AllowedSecTypes []string
	AllowedSymbols  []string
	BlockedSymbols  []string
	// AllowedAccounts restricts the Order.Account field (empty = anything).
	AllowedAccounts []string
	// AllowReducingWhenHalted lets orders that only reduce an existing
	// position through while halted (e.g. to flatten after a loss).
	AllowReducingWhenHalted bool
}

// StateSource gives the manager the current positions and open orders.
// *ibgo.IB satisfies it.
type StateSource interface {
	Positions() []account.Position
	OpenTrades() []*order.Trade
}

// PriceSource returns a reference price for a contract (e.g. last or
// midpoint from a ticker). ok=false when unknown.
type PriceSource func(con *contract.Contract) (price float64, ok bool)

// Option configures a Manager.
type Option func(*Manager)

// WithPriceSource sets the reference price source used for market-order
// notional and for the price collar.
func WithPriceSource(p PriceSource) Option {
	return func(m *Manager) { m.price = p }
}

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) Option {
	return func(m *Manager) { m.now = now }
}

// Manager enforces Limits. It is safe for concurrent use.
type Manager struct {
	mu     sync.Mutex
	limits Limits
	state  StateSource
	price  PriceSource
	now    func() time.Time

	halted     bool
	haltReason string

	submits []time.Time
	recent  []recentOrder

	pnlDay      string
	realizedDay float64
	seenExec    map[string]bool
}

type recentOrder struct {
	at  time.Time
	sig string
}

// New creates a Manager. state may be nil if no position/open-order based
// limit is configured.
func New(limits Limits, state StateSource, opts ...Option) *Manager {
	m := &Manager{limits: limits, state: state, now: time.Now, seenExec: map[string]bool{}}
	for _, o := range opts {
		o(m)
	}
	if m.limits.OrderWindow <= 0 {
		m.limits.OrderWindow = time.Second
	}
	return m
}

// Limits returns the active limits.
func (m *Manager) Limits() Limits {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limits
}

// SetLimits replaces the limits (e.g. intraday tightening).
func (m *Manager) SetLimits(l Limits) {
	m.mu.Lock()
	if l.OrderWindow <= 0 {
		l.OrderWindow = time.Second
	}
	m.limits = l
	m.mu.Unlock()
}

// Halt activates the kill switch: new orders are rejected until Resume.
func (m *Manager) Halt(reason string) {
	m.mu.Lock()
	m.halted = true
	m.haltReason = reason
	m.mu.Unlock()
}

// Resume clears the kill switch.
func (m *Manager) Resume() {
	m.mu.Lock()
	m.halted = false
	m.haltReason = ""
	m.mu.Unlock()
}

// Halted reports whether trading is halted, and why.
func (m *Manager) Halted() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.halted, m.haltReason
}

// RealizedToday returns today's realized P&L seen via commission reports.
func (m *Manager) RealizedToday() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pnlDay != m.now().Format("2006-01-02") {
		return 0
	}
	return m.realizedDay
}

// OnCommissionReport accumulates realized P&L and trips the daily loss
// limit. It implements ibgo.CommissionObserver.
func (m *Manager) OnCommissionReport(_ *order.Trade, fill *order.Fill) {
	if fill == nil || fill.CommissionReport == nil || fill.Execution == nil {
		return
	}
	cr := fill.CommissionReport
	m.mu.Lock()
	defer m.mu.Unlock()
	id := fill.Execution.ExecID
	if id != "" {
		if m.seenExec[id] {
			return
		}
		m.seenExec[id] = true
	}
	day := m.now().Format("2006-01-02")
	if day != m.pnlDay {
		m.pnlDay = day
		m.realizedDay = 0
		m.seenExec = map[string]bool{id: true}
	}
	if pnl := cr.RealizedPNL; isSet(pnl) {
		m.realizedDay += pnl
	}
	if m.limits.MaxDailyLoss > 0 && m.realizedDay <= -m.limits.MaxDailyLoss && !m.halted {
		m.halted = true
		m.haltReason = fmt.Sprintf("%s: realized %.2f reached limit -%.2f", RuleDailyLoss, m.realizedDay, m.limits.MaxDailyLoss)
	}
}

// CheckOrder validates an order. It implements ibgo.PreTradeChecker.
// isModify is true when the order replaces a live one with the same id.
func (m *Manager) CheckOrder(con *contract.Contract, ord *order.Order, isModify bool) error {
	return m.check(con, ord, isModify, 0)
}

// CheckBracketLeg validates one leg of a new bracket order. acceptedLegs
// legs of the same bracket were accepted before it and count as open orders,
// although they are not sent yet. It implements ibgo.BracketChecker.
func (m *Manager) CheckBracketLeg(con *contract.Contract, ord *order.Order, acceptedLegs int) error {
	return m.check(con, ord, false, acceptedLegs)
}

// check implements CheckOrder; pendingOpen orders not yet known to the
// client count towards MaxOpenOrders.
func (m *Manager) check(con *contract.Contract, ord *order.Order, isModify bool, pendingOpen int) error {
	positions, open := m.brokerState()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkOrderLocked(con, ord, isModify, pendingOpen, positions, open)
}

// CheckBracket checks a linked bracket as one submission. Each accepted leg
// uses an open-order slot. A rejected group consumes no rate or duplicate budget.
func (m *Manager) CheckBracket(con *contract.Contract, b *order.BracketOrder) error {
	if b == nil {
		return &Violation{RuleInvalidOrder, "missing bracket"}
	}
	positions, open := m.brokerState()
	m.mu.Lock()
	defer m.mu.Unlock()
	// Window pruning edits these slices in place. Stage the changes until
	// every leg passes, so a rejected child doesn't count an unsent parent.
	submits, recent := m.submits, m.recent
	m.submits, m.recent = slices.Clone(submits), slices.Clone(recent)
	names := [...]string{"parent", "take-profit", "stop-loss"}
	for i, leg := range b.Orders() {
		if err := m.checkOrderLocked(con, leg, false, i, positions, open); err != nil {
			m.submits, m.recent = submits, recent
			return fmt.Errorf("%s leg: %w", names[i], err)
		}
	}
	return nil
}

// Read broker state before taking the risk manager lock.
func (m *Manager) brokerState() ([]account.Position, []*order.Trade) {
	if m.state == nil {
		return nil, nil
	}
	return m.state.Positions(), m.state.OpenTrades()
}

func (m *Manager) checkOrderLocked(con *contract.Contract, ord *order.Order, isModify bool, pendingOpen int, positions []account.Position, open []*order.Trade) error {
	if con == nil || ord == nil {
		return &Violation{RuleInvalidOrder, "missing contract or order"}
	}
	l := m.limits
	now := m.now()

	if ord.Action != "BUY" && ord.Action != "SELL" {
		return &Violation{RuleInvalidOrder, fmt.Sprintf("action must be BUY or SELL, got %q", ord.Action)}
	}
	qty := ord.TotalQuantity
	if qty <= 0 || math.IsNaN(qty) || math.IsInf(qty, 0) {
		if !(qty == 0 && isSet(ord.CashQty) && ord.CashQty > 0) {
			return &Violation{RuleInvalidOrder, fmt.Sprintf("quantity must be positive, got %g", qty)}
		}
	}

	others := excludeSelf(open, ord, isModify)
	pos := positionFor(positions, con, ord.Account)
	openBuy, openSell := openExposure(others, con)
	reduceOnly := qty > 0 && ord.ParentID == 0 && reducesWithOpen(pos, openBuy, openSell, ord.Action, qty)

	if m.halted {
		allowed := l.AllowReducingWhenHalted && reduceOnly
		if !allowed {
			return &Violation{RuleHalted, "trading halted: " + m.haltReason}
		}
	}

	if len(l.AllowedSecTypes) > 0 && !contains(l.AllowedSecTypes, con.SecType) {
		return &Violation{RuleSecType, fmt.Sprintf("secType %q not allowed", con.SecType)}
	}
	sym := strings.ToUpper(con.Symbol)
	if contains(upper(l.BlockedSymbols), sym) {
		return &Violation{RuleSymbol, fmt.Sprintf("symbol %s is blocked", sym)}
	}
	if len(l.AllowedSymbols) > 0 && !contains(upper(l.AllowedSymbols), sym) {
		return &Violation{RuleSymbol, fmt.Sprintf("symbol %s not allowed", sym)}
	}
	if len(l.AllowedAccounts) > 0 && !contains(l.AllowedAccounts, ord.Account) {
		return &Violation{RuleAccount, fmt.Sprintf("account %q not allowed", ord.Account)}
	}

	if l.MaxOrderQuantity > 0 {
		if qty == 0 {
			return &Violation{RuleOrderQuantity, "share quantity is unknown for a cash-quantity order"}
		}
		if qty > l.MaxOrderQuantity {
			return &Violation{RuleOrderQuantity, fmt.Sprintf("quantity %g exceeds max %g", qty, l.MaxOrderQuantity)}
		}
	}

	refPrice, haveRef := 0.0, false
	if m.price != nil {
		refPrice, haveRef = m.price(con)
		haveRef = haveRef && refPrice > 0 && isSet(refPrice)
	}

	if l.MaxOrderNotional > 0 {
		px, ok := orderPrice(ord)
		if !ok && haveRef {
			px, ok = refPrice, true
		}
		if !ok {
			return &Violation{RuleOrderNotional, "cannot determine order price for notional check"}
		}
		notional := math.Abs(qty * px * multiplier(con))
		if isSet(ord.CashQty) && ord.CashQty > notional {
			notional = ord.CashQty
		}
		if notional > l.MaxOrderNotional {
			return &Violation{RuleOrderNotional, fmt.Sprintf("notional %.2f exceeds max %.2f", notional, l.MaxOrderNotional)}
		}
	}

	if l.PriceCollarPct > 0 && haveRef {
		for _, p := range []float64{ord.LmtPrice} {
			if !isSet(p) || p == 0 {
				continue
			}
			dev := math.Abs(p-refPrice) / refPrice * 100
			if dev > l.PriceCollarPct {
				return &Violation{RulePriceCollar, fmt.Sprintf("limit %g is %.2f%% from reference %g (max %.2f%%)", p, dev, refPrice, l.PriceCollarPct)}
			}
		}
	}

	if l.MaxOpenOrders > 0 && !isModify {
		if m.state == nil {
			return &Violation{RuleOpenOrders, "open orders unavailable without a state source"}
		}
		if count := len(others) + pendingOpen; count >= l.MaxOpenOrders {
			return &Violation{RuleOpenOrders, fmt.Sprintf("%d open orders, max %d", count, l.MaxOpenOrders)}
		}
	}

	if l.MaxPositionQuantity > 0 && ord.ParentID == 0 {
		if m.state == nil {
			return &Violation{RulePosition, "positions unavailable without a state source"}
		}
		if qty == 0 {
			return &Violation{RulePosition, "position exposure is unknown for a cash-quantity order"}
		}
		long := pos + openBuy
		short := pos - openSell
		if ord.Action == "BUY" {
			long += qty
		} else {
			short -= qty
		}
		worst := math.Max(math.Abs(long), math.Abs(short))
		// An order that only shrinks the position (together with the open
		// orders on the same side) is always acceptable.
		if worst > l.MaxPositionQuantity && !reduceOnly {
			return &Violation{RulePosition, fmt.Sprintf("worst-case position %g exceeds max %g (position %g, open buy %g, open sell %g)",
				worst, l.MaxPositionQuantity, pos, openBuy, openSell)}
		}
	}

	if l.DuplicateWindow > 0 && !isModify {
		sig := signature(con, ord)
		cutoff := now.Add(-l.DuplicateWindow)
		kept := m.recent[:0]
		dup := false
		for _, r := range m.recent {
			if r.at.After(cutoff) {
				kept = append(kept, r)
				if r.sig == sig {
					dup = true
				}
			}
		}
		m.recent = kept
		if dup {
			return &Violation{RuleDuplicate, fmt.Sprintf("identical order submitted within %v", l.DuplicateWindow)}
		}
	}

	if l.MaxOrdersPerWindow > 0 {
		cutoff := now.Add(-l.OrderWindow)
		kept := m.submits[:0]
		for _, ts := range m.submits {
			if ts.After(cutoff) {
				kept = append(kept, ts)
			}
		}
		m.submits = kept
		if len(m.submits) >= l.MaxOrdersPerWindow {
			return &Violation{RuleOrderRate, fmt.Sprintf("%d orders within %v, max %d", len(m.submits), l.OrderWindow, l.MaxOrdersPerWindow)}
		}
	}

	// Accepted: record for rate and duplicate tracking.
	if l.MaxOrdersPerWindow > 0 {
		m.submits = append(m.submits, now)
	}
	if l.DuplicateWindow > 0 && !isModify {
		m.recent = append(m.recent, recentOrder{at: now, sig: signature(con, ord)})
	}
	return nil
}

// --- helpers ---

func isSet(v float64) bool {
	return v != math.MaxFloat64 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func orderPrice(o *order.Order) (float64, bool) {
	if isSet(o.LmtPrice) && o.LmtPrice > 0 {
		return o.LmtPrice, true
	}
	if isSet(o.AuxPrice) && o.AuxPrice > 0 {
		return o.AuxPrice, true
	}
	return 0, false
}

func multiplier(c *contract.Contract) float64 {
	if c.Multiplier == "" {
		return 1
	}
	m, err := strconv.ParseFloat(c.Multiplier, 64)
	if err != nil || m <= 0 {
		return 1
	}
	return m
}

// openExposure sums the remaining quantity of open orders on con, per side.
// Bracket children (ParentID != 0) only close the parent's exposure and are
// not counted.
func openExposure(open []*order.Trade, con *contract.Contract) (buy, sell float64) {
	for _, t := range open {
		if t.Order == nil || t.Order.ParentID != 0 || !sameContract(t.Contract, con) {
			continue
		}
		rem := t.RemainingQty()
		if rem <= 0 {
			continue
		}
		if t.Order.Action == "BUY" {
			buy += rem
		} else {
			sell += rem
		}
	}
	return buy, sell
}

// reducesWithOpen reports whether the order, together with the open orders
// on the same side, can only shrink the position without crossing zero.
func reducesWithOpen(pos, openBuy, openSell float64, action string, qty float64) bool {
	switch {
	case pos > 0 && action == "SELL":
		return pos-openSell-qty >= 0
	case pos < 0 && action == "BUY":
		return pos+openBuy+qty <= 0
	}
	return false
}

func sameContract(a, b *contract.Contract) bool {
	if a == nil || b == nil {
		return false
	}
	if a.ConID != 0 && b.ConID != 0 {
		return a.ConID == b.ConID
	}
	return a.Symbol == b.Symbol && a.SecType == b.SecType &&
		a.LastTradeDateOrContractMonth == b.LastTradeDateOrContractMonth &&
		a.Strike == b.Strike && a.Right == b.Right && a.Currency == b.Currency
}

func positionFor(positions []account.Position, con *contract.Contract, acct string) float64 {
	var total float64
	for _, p := range positions {
		if acct != "" && p.Account != acct {
			continue
		}
		if sameContract(p.Contract, con) {
			total += p.Position
		}
	}
	return total
}

func excludeSelf(open []*order.Trade, ord *order.Order, isModify bool) []*order.Trade {
	if !isModify {
		return open
	}
	out := make([]*order.Trade, 0, len(open))
	for _, t := range open {
		if t.Order != nil && t.Order.OrderID == ord.OrderID && t.Order.ClientID == ord.ClientID {
			continue
		}
		out = append(out, t)
	}
	return out
}

func signature(c *contract.Contract, o *order.Order) string {
	return fmt.Sprintf("%d|%s|%s|%s|%g|%s|%g|%g|%s", c.ConID, c.Symbol, c.SecType, o.Action, o.TotalQuantity, o.OrderType, o.LmtPrice, o.AuxPrice, o.Account)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func upper(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = strings.ToUpper(s)
	}
	return out
}
