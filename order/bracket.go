package order

import (
	"fmt"
	"strconv"
)

// NewBracket builds a bracket: a limit entry plus a take-profit limit and a
// stop-loss stop on the opposite side, both children of the entry. Order ids
// are left at 0 (assign them, or let IB.BracketOrder do it); transmit flags are
// set so that only the last leg (stop-loss) releases the whole group.
//
// Prices are validated for the side: for BUY, takeProfit > limit > stopLoss;
// for SELL, takeProfit < limit < stopLoss.
func NewBracket(action string, quantity, limitPrice, takeProfitPrice, stopLossPrice float64) (*BracketOrder, error) {
	var reverse string
	switch action {
	case "BUY":
		reverse = "SELL"
		if !(takeProfitPrice > limitPrice && limitPrice > stopLossPrice) {
			return nil, fmt.Errorf("order: BUY bracket needs takeProfit > limit > stopLoss (got %g / %g / %g)", takeProfitPrice, limitPrice, stopLossPrice)
		}
	case "SELL":
		reverse = "BUY"
		if !(takeProfitPrice < limitPrice && limitPrice < stopLossPrice) {
			return nil, fmt.Errorf("order: SELL bracket needs takeProfit < limit < stopLoss (got %g / %g / %g)", takeProfitPrice, limitPrice, stopLossPrice)
		}
	default:
		return nil, fmt.Errorf("order: bracket action must be BUY or SELL, got %q", action)
	}
	if quantity <= 0 {
		return nil, fmt.Errorf("order: bracket quantity must be positive, got %g", quantity)
	}
	if stopLossPrice <= 0 && action == "BUY" {
		return nil, fmt.Errorf("order: BUY bracket stopLoss must be positive, got %g", stopLossPrice)
	}
	if takeProfitPrice <= 0 && action == "SELL" {
		return nil, fmt.Errorf("order: SELL bracket takeProfit must be positive, got %g", takeProfitPrice)
	}

	parent := LimitOrder(action, quantity, limitPrice)
	parent.Transmit = false
	takeProfit := LimitOrder(reverse, quantity, takeProfitPrice)
	takeProfit.Transmit = false
	stopLoss := StopOrder(reverse, quantity, stopLossPrice)
	stopLoss.Transmit = true

	b := &BracketOrder{Parent: parent, TakeProfit: takeProfit, StopLoss: stopLoss}
	return b, nil
}

// Link sets the parent id of the children and the transmit flags. Call it
// after order ids have been assigned.
func (b *BracketOrder) Link() {
	b.Parent.Transmit = false
	b.TakeProfit.Transmit = false
	b.StopLoss.Transmit = true
	b.TakeProfit.ParentID = b.Parent.OrderID
	b.StopLoss.ParentID = b.Parent.OrderID
}

// OneCancelsAll puts orders in the same OCA group: when one fills, the others
// are cancelled (ocaType 1) or reduced (2, 3). It returns the same slice.
func OneCancelsAll(orders []*Order, ocaGroup string, ocaType int) []*Order {
	for _, o := range orders {
		o.OcaGroup = ocaGroup
		o.OcaType = ocaType
	}
	return orders
}

// ParseValue parses a numeric string field of OrderState (margins, equity).
// It returns false for empty or unset (UNSET_DOUBLE) values.
func ParseValue(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v == unsetDouble {
		return 0, false
	}
	return v, true
}
