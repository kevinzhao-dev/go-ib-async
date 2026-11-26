// Package journal implements a persistent, append-only order journal.
//
// Every order lifecycle event (submit, modify, cancel request, status change,
// fill, commission, error) is written as one JSON line. The submit record is
// written before the order is sent (write-ahead), so after a crash the journal
// always knows about every order that may have reached the broker. Load and
// Replay rebuild the last known state of each order; Reconcile compares it
// with what the broker reports after reconnecting.
//
// A *Journal satisfies ibgo.OrderJournal:
//
//	j, err := journal.Open("orders-2026-11-26.jsonl", journal.Options{})
//	ib.SetJournal(j)
package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

// Record types.
const (
	TypeSubmit        = "submit"
	TypeModify        = "modify"
	TypeCancelRequest = "cancel_request"
	TypeStatus        = "status"
	TypeFill          = "fill"
	TypeCommission    = "commission"
	TypeError         = "error"
)

// ContractSnapshot is the subset of a contract stored in the journal.
type ContractSnapshot struct {
	ConID                        int64   `json:"conId,omitempty"`
	Symbol                       string  `json:"symbol,omitempty"`
	SecType                      string  `json:"secType,omitempty"`
	LastTradeDateOrContractMonth string  `json:"lastTradeDate,omitempty"`
	Strike                       float64 `json:"strike,omitempty"`
	Right                        string  `json:"right,omitempty"`
	Multiplier                   string  `json:"multiplier,omitempty"`
	Exchange                     string  `json:"exchange,omitempty"`
	Currency                     string  `json:"currency,omitempty"`
	LocalSymbol                  string  `json:"localSymbol,omitempty"`
}

// OrderSnapshot is the subset of an order stored in the journal.
// Unset prices are stored as null.
type OrderSnapshot struct {
	Action        string   `json:"action"`
	TotalQuantity float64  `json:"totalQuantity"`
	OrderType     string   `json:"orderType"`
	LmtPrice      *float64 `json:"lmtPrice,omitempty"`
	AuxPrice      *float64 `json:"auxPrice,omitempty"`
	TIF           string   `json:"tif,omitempty"`
	Account       string   `json:"account,omitempty"`
	OrderRef      string   `json:"orderRef,omitempty"`
	OcaGroup      string   `json:"ocaGroup,omitempty"`
	ParentID      int64    `json:"parentId,omitempty"`
	Transmit      bool     `json:"transmit"`
	OutsideRTH    bool     `json:"outsideRth,omitempty"`
}

// StatusSnapshot is an order status as reported by TWS.
type StatusSnapshot struct {
	Status       string  `json:"status"`
	Filled       float64 `json:"filled"`
	Remaining    float64 `json:"remaining"`
	AvgFillPrice float64 `json:"avgFillPrice,omitempty"`
}

// FillSnapshot is one execution.
type FillSnapshot struct {
	ExecID   string    `json:"execId"`
	Time     time.Time `json:"time"`
	Side     string    `json:"side"`
	Shares   float64   `json:"shares"`
	Price    float64   `json:"price"`
	CumQty   float64   `json:"cumQty,omitempty"`
	AvgPrice float64   `json:"avgPrice,omitempty"`
	Exchange string    `json:"exchange,omitempty"`
	Account  string    `json:"account,omitempty"`
}

// CommissionSnapshot is the commission report of one execution.
type CommissionSnapshot struct {
	ExecID      string  `json:"execId"`
	Commission  float64 `json:"commission"`
	Currency    string  `json:"currency,omitempty"`
	RealizedPNL float64 `json:"realizedPnl,omitempty"`
}

// Record is one journal line.
type Record struct {
	Seq        int64               `json:"seq"`
	Time       time.Time           `json:"time"`
	Type       string              `json:"type"`
	ClientID   int64               `json:"clientId"`
	OrderID    int64               `json:"orderId"`
	PermID     int64               `json:"permId,omitempty"`
	Contract   *ContractSnapshot   `json:"contract,omitempty"`
	Order      *OrderSnapshot      `json:"order,omitempty"`
	Status     *StatusSnapshot     `json:"status,omitempty"`
	Fill       *FillSnapshot       `json:"fill,omitempty"`
	Commission *CommissionSnapshot `json:"commission,omitempty"`
	ErrorCode  int                 `json:"errorCode,omitempty"`
	Message    string              `json:"message,omitempty"`
}

// Key identifies an order the same way the client does: (clientId, orderId)
// for API orders, (0, permId) for manual TWS orders.
type Key struct {
	ClientID int64
	OrderID  int64
}

// KeyOf returns the key of a record.
func (r Record) KeyOf() Key {
	if r.OrderID <= 0 {
		return Key{ClientID: 0, OrderID: r.PermID}
	}
	return Key{ClientID: r.ClientID, OrderID: r.OrderID}
}

// Options configures a Journal.
type Options struct {
	// NoSync skips fsync after each record. Faster, but the last records may
	// be lost on power failure (not on a process crash).
	NoSync bool
	// Now overrides the clock (tests).
	Now func() time.Time
}

// ErrClosed is returned when writing to a closed journal.
var ErrClosed = errors.New("journal: closed")

// Journal is an append-only JSONL order journal. It is safe for concurrent use.
type Journal struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	seq    int64
	opts   Options
	failed error // sticky: once a write fails the journal refuses new records
	closed bool
}

// Open opens (creating if needed) the journal at path. An incomplete last
// line left by a crash during a write is truncated; sequence numbers continue
// from the last complete record.
func Open(path string, opts Options) (*Journal, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("journal: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	lastSeq, goodLen, err := scanTail(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Truncate(goodLen); err != nil {
		f.Close()
		return nil, fmt.Errorf("journal: truncate partial record: %w", err)
	}
	if _, err := f.Seek(goodLen, io.SeekStart); err != nil {
		f.Close()
		return nil, fmt.Errorf("journal: %w", err)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Journal{path: path, f: f, seq: lastSeq, opts: opts}, nil
}

// scanTail returns the last sequence number and the byte length of the
// complete (newline-terminated, valid JSON) prefix of the file.
func scanTail(f *os.File) (int64, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, fmt.Errorf("journal: %w", err)
	}
	br := bufio.NewReaderSize(f, 64*1024)
	var off, goodLen, lastSeq int64
	line := 0
	for {
		b, err := br.ReadBytes('\n')
		if len(b) > 0 && b[len(b)-1] == '\n' {
			line++
			var rec Record
			if jerr := json.Unmarshal(bytes.TrimSpace(b), &rec); jerr != nil {
				if len(bytes.TrimSpace(b)) != 0 {
					return 0, 0, fmt.Errorf("journal: corrupt record at line %d: %w", line, jerr)
				}
			} else {
				lastSeq = rec.Seq
			}
			off += int64(len(b))
			goodLen = off
		}
		if err == io.EOF {
			return lastSeq, goodLen, nil
		}
		if err != nil {
			return 0, 0, fmt.Errorf("journal: %w", err)
		}
	}
}

// Path returns the journal file path.
func (j *Journal) Path() string { return j.path }

// Err returns the sticky write error, if any.
func (j *Journal) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.failed
}

// Append writes one record, assigning its sequence number and (if zero) time.
// After any write failure the journal refuses further records, so that order
// submission (which requires a journal record) fails closed.
func (j *Journal) Append(rec Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	if j.failed != nil {
		return fmt.Errorf("journal: previous write failed: %w", j.failed)
	}
	rec.Seq = j.seq + 1
	if rec.Time.IsZero() {
		rec.Time = j.opts.Now()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("journal: encode: %w", err)
	}
	b = append(b, '\n')
	if _, err := j.f.Write(b); err != nil {
		j.failed = err
		return fmt.Errorf("journal: write: %w", err)
	}
	if !j.opts.NoSync {
		if err := j.f.Sync(); err != nil {
			j.failed = err
			return fmt.Errorf("journal: sync: %w", err)
		}
	}
	j.seq = rec.Seq
	return nil
}

// Close flushes and closes the journal.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	syncErr := j.f.Sync()
	closeErr := j.f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// --- ibgo.OrderJournal implementation ---

// RecordSubmit writes the write-ahead record of a new or modified order.
func (j *Journal) RecordSubmit(con *contract.Contract, ord *order.Order, isModify bool) error {
	typ := TypeSubmit
	if isModify {
		typ = TypeModify
	}
	return j.Append(Record{
		Type:     typ,
		ClientID: ord.ClientID,
		OrderID:  ord.OrderID,
		PermID:   ord.PermID,
		Contract: snapshotContract(con),
		Order:    snapshotOrder(ord),
	})
}

// RecordCancelRequest records that cancellation was requested.
func (j *Journal) RecordCancelRequest(trade *order.Trade) error {
	rec := tradeRecord(trade, TypeCancelRequest)
	return j.Append(rec)
}

// RecordStatus records the trade's current status.
func (j *Journal) RecordStatus(trade *order.Trade) error {
	if trade == nil {
		return nil
	}
	rec := tradeRecord(trade, TypeStatus)
	st := trade.StatusSnapshot()
	rec.Status = &StatusSnapshot{
		Status:       st.Status,
		Filled:       st.Filled,
		Remaining:    st.Remaining,
		AvgFillPrice: st.AvgFillPrice,
	}
	return j.Append(rec)
}

// RecordFill records an execution.
func (j *Journal) RecordFill(trade *order.Trade, fill *order.Fill) error {
	if fill == nil || fill.Execution == nil {
		return nil
	}
	rec := fillRecord(trade, fill, TypeFill)
	ex := fill.Execution
	rec.Fill = &FillSnapshot{
		ExecID:   ex.ExecID,
		Time:     ex.Time,
		Side:     ex.Side,
		Shares:   ex.Shares,
		Price:    ex.Price,
		CumQty:   ex.CumQty,
		AvgPrice: ex.AvgPrice,
		Exchange: ex.Exchange,
		Account:  ex.AcctNumber,
	}
	if trade == nil {
		rec.Contract = snapshotContract(fill.Contract)
	}
	return j.Append(rec)
}

// RecordCommission records the commission report of an execution.
func (j *Journal) RecordCommission(trade *order.Trade, fill *order.Fill) error {
	if fill == nil || fill.CommissionReport == nil {
		return nil
	}
	rec := fillRecord(trade, fill, TypeCommission)
	cr := fill.CommissionReport
	rec.Commission = &CommissionSnapshot{
		ExecID:      cr.ExecID,
		Commission:  finite(cr.Commission),
		Currency:    cr.Currency,
		RealizedPNL: finite(cr.RealizedPNL),
	}
	return j.Append(rec)
}

// RecordError records an error/warning for an order.
func (j *Journal) RecordError(trade *order.Trade, code int, msg string) error {
	rec := tradeRecord(trade, TypeError)
	rec.ErrorCode = code
	rec.Message = msg
	return j.Append(rec)
}

func tradeRecord(trade *order.Trade, typ string) Record {
	rec := Record{Type: typ}
	if trade == nil || trade.Order == nil {
		return rec
	}
	st := trade.StatusSnapshot()
	rec.ClientID = trade.Order.ClientID
	if rec.ClientID == 0 {
		rec.ClientID = st.ClientID
	}
	rec.OrderID = trade.Order.OrderID
	rec.PermID = trade.PermID()
	return rec
}

func fillRecord(trade *order.Trade, fill *order.Fill, typ string) Record {
	if trade != nil {
		return tradeRecord(trade, typ)
	}
	ex := fill.Execution
	return Record{Type: typ, ClientID: ex.ClientID, OrderID: ex.OrderID, PermID: ex.PermID}
}

func snapshotContract(c *contract.Contract) *ContractSnapshot {
	if c == nil {
		return nil
	}
	return &ContractSnapshot{
		ConID:                        c.ConID,
		Symbol:                       c.Symbol,
		SecType:                      c.SecType,
		LastTradeDateOrContractMonth: c.LastTradeDateOrContractMonth,
		Strike:                       finite(c.Strike),
		Right:                        c.Right,
		Multiplier:                   c.Multiplier,
		Exchange:                     c.Exchange,
		Currency:                     c.Currency,
		LocalSymbol:                  c.LocalSymbol,
	}
}

func snapshotOrder(o *order.Order) *OrderSnapshot {
	return &OrderSnapshot{
		Action:        o.Action,
		TotalQuantity: o.TotalQuantity,
		OrderType:     o.OrderType,
		LmtPrice:      priceOrNil(o.LmtPrice),
		AuxPrice:      priceOrNil(o.AuxPrice),
		TIF:           o.TIF,
		Account:       o.Account,
		OrderRef:      o.OrderRef,
		OcaGroup:      o.OcaGroup,
		ParentID:      o.ParentID,
		Transmit:      o.Transmit,
		OutsideRTH:    o.OutsideRTH,
	}
}

func priceOrNil(v float64) *float64 {
	if v == math.MaxFloat64 || math.IsInf(v, 0) || math.IsNaN(v) {
		return nil
	}
	return &v
}

// finite maps IB sentinels and non-finite values (which JSON cannot encode)
// to zero.
func finite(v float64) float64 {
	if v == math.MaxFloat64 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}
	return v
}
