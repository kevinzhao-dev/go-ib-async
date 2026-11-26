package ibgo

import (
	"log"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

// PreTradeChecker validates an order before it is sent to TWS.
// Returning a non-nil error blocks the order; nothing is sent.
//
// CheckOrder is called with the IB client's order-placement lock held, so
// implementations must not call PlaceOrder/CancelOrder themselves. They may
// call read-only accessors such as Positions and OpenTrades.
type PreTradeChecker interface {
	CheckOrder(con *contract.Contract, ord *order.Order, isModify bool) error
}

// CommissionObserver is optionally implemented by a PreTradeChecker that
// wants to see realized P&L (e.g. for a daily loss limit). It is called on
// the reader goroutine for every commission report of a known execution.
type CommissionObserver interface {
	OnCommissionReport(trade *order.Trade, fill *order.Fill)
}

// OrderJournal persists the order lifecycle.
//
// RecordSubmit is a write-ahead record: it is called before an order is sent
// and an error aborts the send, so an order never reaches the broker without
// a durable record. All other methods are called on the reader goroutine
// after the fact; their errors are logged but cannot undo what the broker did.
// trade may be nil for fills/commissions of orders this client doesn't know.
type OrderJournal interface {
	RecordSubmit(con *contract.Contract, ord *order.Order, isModify bool) error
	RecordCancelRequest(trade *order.Trade) error
	RecordStatus(trade *order.Trade) error
	RecordFill(trade *order.Trade, fill *order.Fill) error
	RecordCommission(trade *order.Trade, fill *order.Fill) error
	RecordError(trade *order.Trade, code int, msg string) error
}

// SetRiskManager installs (or, with nil, removes) the pre-trade checker
// consulted by PlaceOrder and PlaceBracketOrder.
func (ib *IB) SetRiskManager(c PreTradeChecker) {
	ib.hooksMu.Lock()
	ib.risk = c
	ib.hooksMu.Unlock()
}

// SetJournal installs (or, with nil, removes) the order journal.
func (ib *IB) SetJournal(j OrderJournal) {
	ib.hooksMu.Lock()
	ib.journal = j
	ib.hooksMu.Unlock()
}

func (ib *IB) riskChecker() PreTradeChecker {
	ib.hooksMu.RLock()
	defer ib.hooksMu.RUnlock()
	return ib.risk
}

func (ib *IB) orderJournal() OrderJournal {
	ib.hooksMu.RLock()
	defer ib.hooksMu.RUnlock()
	return ib.journal
}

func logJournalErr(what string, err error) {
	if err != nil {
		log.Printf("ibgo: journal %s failed: %v", what, err)
	}
}

func (ib *IB) journalStatus(trade *order.Trade) {
	if j := ib.orderJournal(); j != nil {
		logJournalErr("status", j.RecordStatus(trade))
	}
}

func (ib *IB) journalError(trade *order.Trade, code int, msg string) {
	if j := ib.orderJournal(); j != nil {
		logJournalErr("error", j.RecordError(trade, code, msg))
	}
}

func (ib *IB) journalFill(trade *order.Trade, fill *order.Fill) {
	if j := ib.orderJournal(); j != nil {
		logJournalErr("fill", j.RecordFill(trade, fill))
	}
}

func (ib *IB) notifyCommission(trade *order.Trade, fill *order.Fill) {
	if j := ib.orderJournal(); j != nil {
		logJournalErr("commission", j.RecordCommission(trade, fill))
	}
	if obs, ok := ib.riskChecker().(CommissionObserver); ok {
		obs.OnCommissionReport(trade, fill)
	}
}
