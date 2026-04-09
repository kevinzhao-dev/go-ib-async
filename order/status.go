package order

import "math"

// OrderStatus tracks the current state of an order.
type OrderStatus struct {
	OrderID      int64
	Status       string
	Filled       float64
	Remaining    float64
	AvgFillPrice float64
	PermID       int64
	ParentID     int64
	LastFillPrice float64
	ClientID     int64
	WhyHeld      string
	MktCapPrice  float64
}

// Total returns the total order size (filled + remaining).
func (s *OrderStatus) Total() float64 {
	return s.Filled + s.Remaining
}

// Status string constants.
const (
	StatusPendingSubmit  = "PendingSubmit"
	StatusPendingCancel  = "PendingCancel"
	StatusPreSubmitted   = "PreSubmitted"
	StatusSubmitted      = "Submitted"
	StatusApiPending     = "ApiPending"
	StatusApiCancelled   = "ApiCancelled"
	StatusApiUpdate      = "ApiUpdate"
	StatusCancelled      = "Cancelled"
	StatusFilled         = "Filled"
	StatusInactive       = "Inactive"
	StatusValidationError = "ValidationError"
)

// DoneStates are terminal order states.
var DoneStates = map[string]bool{
	StatusFilled:       true,
	StatusCancelled:    true,
	StatusApiCancelled: true,
	StatusInactive:     true,
}

// ActiveStates are states where the order could execute.
var ActiveStates = map[string]bool{
	StatusPendingSubmit:  true,
	StatusApiPending:     true,
	StatusPreSubmitted:   true,
	StatusSubmitted:      true,
	StatusValidationError: true,
	StatusApiUpdate:      true,
}

// WaitingStates are states before the order is live.
var WaitingStates = map[string]bool{
	StatusPendingSubmit: true,
	StatusApiPending:    true,
	StatusPreSubmitted:  true,
}

// WorkingStates are states where the order is live at the exchange.
var WorkingStates = map[string]bool{
	StatusSubmitted:      true,
	StatusValidationError: true,
	StatusApiUpdate:      true,
}

// OrderState holds what-if and pre-trade margin impact information.
type OrderState struct {
	Status               string
	InitMarginBefore     string
	MaintMarginBefore    string
	EquityWithLoanBefore string
	InitMarginChange     string
	MaintMarginChange    string
	EquityWithLoanChange string
	InitMarginAfter      string
	MaintMarginAfter     string
	EquityWithLoanAfter  string
	Commission           float64
	MinCommission        float64
	MaxCommission        float64
	CommissionCurrency   string
	WarningText          string
	CompletedTime        string
	CompletedStatus      string
}

// NewOrderState returns an OrderState with UNSET commissions.
func NewOrderState() *OrderState {
	return &OrderState{
		Commission:    math.MaxFloat64,
		MinCommission: math.MaxFloat64,
		MaxCommission: math.MaxFloat64,
	}
}
