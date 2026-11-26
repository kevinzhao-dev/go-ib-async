package order

import (
	"strconv"
	"sync"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/event"
)

// TradeLogEntry records a status change or event for a trade.
type TradeLogEntry struct {
	Time      time.Time
	Status    string
	Message   string
	ErrorCode int
}

// Fill combines contract, execution, commission and time for a single fill.
type Fill struct {
	Contract         *contract.Contract
	Execution        *Execution
	CommissionReport *CommissionReport
	Time             time.Time
}

// Execution holds details of a trade execution.
type Execution struct {
	ExecID               string
	Time                 time.Time
	AcctNumber           string
	Exchange             string
	Side                 string
	Shares               float64
	Price                float64
	PermID               int64
	ClientID             int64
	OrderID              int64
	Liquidation          int
	CumQty               float64
	AvgPrice             float64
	OrderRef             string
	EvRule               string
	EvMultiplier         float64
	ModelCode            string
	LastLiquidity        int
	PendingPriceRevision bool
}

// CommissionReport holds commission and P&L for an execution.
type CommissionReport struct {
	ExecID              string
	Commission          float64
	Currency            string
	RealizedPNL         float64
	Yield               float64
	YieldRedemptionDate int
}

// ExecutionFilter is used to query executions.
type ExecutionFilter struct {
	ClientID int64
	AcctCode string
	Time     string
	Symbol   string
	SecType  string
	Exchange string
	Side     string
}

// Trade tracks an order, its status, and all its fills.
//
// Concurrency: a Trade is updated by the client's reader goroutine while the
// application reads it from other goroutines. Use the accessor methods
// (Status, StatusSnapshot, FillsSnapshot, LogSnapshot, FilledQty, IsDone, ...)
// which take the trade's internal lock, instead of reading OrderStatus, Fills
// or Log directly. Event callbacks run on the reader goroutine.
type Trade struct {
	mu sync.RWMutex

	Contract      *contract.Contract
	Order         *Order
	OrderStatus   *OrderStatus
	Fills         []*Fill
	Log           []TradeLogEntry
	AdvancedError string

	// Events
	StatusEvent           event.Event[*Trade]
	ModifyEvent           event.Event[*Trade]
	FillEvent             event.Event[*Fill]
	CommissionReportEvent event.Event[*Fill]
	FilledEvent           event.Event[*Trade]
	CancelEvent           event.Event[*Trade]
	CancelledEvent        event.Event[*Trade]
}

// NewTrade creates a Trade with initialized fields.
func NewTrade(c *contract.Contract, o *Order) *Trade {
	return &Trade{
		Contract:    c,
		Order:       o,
		OrderStatus: &OrderStatus{},
	}
}

func (t *Trade) statusLocked() string {
	if t.OrderStatus == nil {
		return ""
	}
	return t.OrderStatus.Status
}

// Status returns the current order status string.
func (t *Trade) Status() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.statusLocked()
}

// StatusSnapshot returns a copy of the current order status.
func (t *Trade) StatusSnapshot() OrderStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.OrderStatus == nil {
		return OrderStatus{}
	}
	return *t.OrderStatus
}

// FillsSnapshot returns a copy of the fill list.
func (t *Trade) FillsSnapshot() []*Fill {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*Fill, len(t.Fills))
	copy(out, t.Fills)
	return out
}

// LogSnapshot returns a copy of the trade log.
func (t *Trade) LogSnapshot() []TradeLogEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]TradeLogEntry, len(t.Log))
	copy(out, t.Log)
	return out
}

// IsWaiting returns true if the order is sent but not yet live.
func (t *Trade) IsWaiting() bool {
	return WaitingStates[t.Status()]
}

// IsWorking returns true if the order is live at the exchange.
func (t *Trade) IsWorking() bool {
	return WorkingStates[t.Status()]
}

// IsActive returns true if the order is eligible for execution.
func (t *Trade) IsActive() bool {
	return ActiveStates[t.Status()]
}

// IsDone returns true if the order is completely filled or cancelled.
func (t *Trade) IsDone() bool {
	return DoneStates[t.Status()]
}

// FilledQty returns the total shares filled.
func (t *Trade) FilledQty() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.filledLocked()
}

func (t *Trade) filledLocked() float64 {
	var total float64
	for _, f := range t.Fills {
		if t.Contract != nil && t.Contract.SecType == "BAG" && f.Contract != nil && f.Contract.SecType != "BAG" {
			continue
		}
		total += f.Execution.Shares
	}
	return total
}

// RemainingQty returns the shares remaining to be filled.
func (t *Trade) RemainingQty() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Order.TotalQuantity - t.filledLocked()
}

// --- Mutators used by the client. Applications normally don't call these. ---

// UpdateOrderStatus applies an orderStatus message using ib_async semantics:
// the log is only appended (and emit reported true) when something actually
// changed, or when a "Submitted" status acknowledges a pending modification.
func (t *Trade) UpdateOrderStatus(st OrderStatus, now time.Time) (oldStatus string, emit bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	oldStatus = t.statusLocked()
	var cur OrderStatus
	if t.OrderStatus != nil {
		cur = *t.OrderStatus
	}
	if st.OrderID == 0 {
		st.OrderID = cur.OrderID
	}
	msg := ""
	switch {
	case cur != st:
		ns := st
		t.OrderStatus = &ns // copy-on-write: old pointers stay internally consistent
	case st.Status == StatusSubmitted && len(t.Log) > 0 && t.Log[len(t.Log)-1].Message == "Modify":
		msg = "Modified"
	default:
		return oldStatus, false
	}
	t.Log = append(t.Log, TradeLogEntry{Time: now, Status: st.Status, Message: msg})
	return oldStatus, true
}

// SetStatus forces the status (e.g. on cancel request or order rejection)
// and appends a log entry. It returns the previous status.
func (t *Trade) SetStatus(status string, now time.Time, msg string, errorCode int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	old := t.statusLocked()
	ns := OrderStatus{}
	if t.OrderStatus != nil {
		ns = *t.OrderStatus
	}
	ns.Status = status
	t.OrderStatus = &ns
	t.Log = append(t.Log, TradeLogEntry{Time: now, Status: status, Message: msg, ErrorCode: errorCode})
	return old
}

// ApplyFinalStatus sets a terminal status learned out-of-band (e.g. from
// completed orders during a resync), with the filled quantity, and logs msg.
// It returns the previous status.
func (t *Trade) ApplyFinalStatus(status string, filled float64, now time.Time, msg string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	old := t.statusLocked()
	ns := OrderStatus{}
	if t.OrderStatus != nil {
		ns = *t.OrderStatus
	}
	ns.Status = status
	if filled >= 0 {
		ns.Filled = filled
		ns.Remaining = t.Order.TotalQuantity - filled
		if ns.Remaining < 0 {
			ns.Remaining = 0
		}
	}
	t.OrderStatus = &ns
	t.Log = append(t.Log, TradeLogEntry{Time: now, Status: status, Message: msg})
	return old
}

// AppendLog appends a log entry.
func (t *Trade) AppendLog(entry TradeLogEntry) {
	t.mu.Lock()
	t.Log = append(t.Log, entry)
	t.mu.Unlock()
}

// AddFill appends a fill and a matching log entry.
func (t *Trade) AddFill(f *Fill, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Fills = append(t.Fills, f)
	t.Log = append(t.Log, TradeLogEntry{
		Time:    now,
		Status:  t.statusLocked(),
		Message: "Fill " + formatQty(f.Execution.Shares) + "@" + formatQty(f.Execution.Price),
	})
}

// UpdateFromOpenOrder copies the fields TWS may change on a live order.
func (t *Trade) UpdateFromOpenOrder(o *Order) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Order.PermID = o.PermID
	t.Order.TotalQuantity = o.TotalQuantity
	t.Order.LmtPrice = o.LmtPrice
	t.Order.AuxPrice = o.AuxPrice
	t.Order.OrderType = o.OrderType
	t.Order.OrderRef = o.OrderRef
}

// SetPermID records the permanent id assigned by TWS.
func (t *Trade) SetPermID(permID int64) {
	t.mu.Lock()
	t.Order.PermID = permID
	t.mu.Unlock()
}

// PermID returns the permanent id assigned by TWS (0 if not yet known).
func (t *Trade) PermID() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Order.PermID
}

// SetAdvancedError records the advanced order reject JSON from TWS.
func (t *Trade) SetAdvancedError(s string) {
	t.mu.Lock()
	t.AdvancedError = s
	t.mu.Unlock()
}

// BracketOrder groups a parent, take-profit, and stop-loss order.
type BracketOrder struct {
	Parent     *Order
	TakeProfit *Order
	StopLoss   *Order
}

// Orders returns the bracket legs in placement order (parent, take-profit,
// stop-loss). The last leg carries Transmit=true and releases the group.
func (b *BracketOrder) Orders() []*Order {
	return []*Order{b.Parent, b.TakeProfit, b.StopLoss}
}

func formatQty(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
