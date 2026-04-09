package order

import (
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
	ExecID             string
	Commission         float64
	Currency           string
	RealizedPNL        float64
	Yield              float64
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
type Trade struct {
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

// IsWaiting returns true if the order is sent but not yet live.
func (t *Trade) IsWaiting() bool {
	return WaitingStates[t.OrderStatus.Status]
}

// IsWorking returns true if the order is live at the exchange.
func (t *Trade) IsWorking() bool {
	return WorkingStates[t.OrderStatus.Status]
}

// IsActive returns true if the order is eligible for execution.
func (t *Trade) IsActive() bool {
	return ActiveStates[t.OrderStatus.Status]
}

// IsDone returns true if the order is completely filled or cancelled.
func (t *Trade) IsDone() bool {
	return DoneStates[t.OrderStatus.Status]
}

// FilledQty returns the total shares filled.
func (t *Trade) FilledQty() float64 {
	var total float64
	for _, f := range t.Fills {
		if t.Contract.SecType == "BAG" && f.Contract.SecType != "BAG" {
			continue
		}
		total += f.Execution.Shares
	}
	return total
}

// RemainingQty returns the shares remaining to be filled.
func (t *Trade) RemainingQty() float64 {
	return t.Order.TotalQuantity - t.FilledQty()
}

// BracketOrder groups a parent, take-profit, and stop-loss order.
type BracketOrder struct {
	Parent     *Order
	TakeProfit *Order
	StopLoss   *Order
}
