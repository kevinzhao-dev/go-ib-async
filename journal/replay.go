package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/order"
)

// Load reads all complete records from a journal file. A trailing partial
// line (crash during a write) is ignored; a malformed complete line is an
// error, because silently skipping it could hide an order.
func Load(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	defer f.Close()
	return Read(f)
}

// Read decodes records from r (see Load).
func Read(r io.Reader) ([]Record, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var out []Record
	line := 0
	for {
		b, err := br.ReadBytes('\n')
		complete := len(b) > 0 && b[len(b)-1] == '\n'
		if complete {
			line++
			trimmed := bytes.TrimSpace(b)
			if len(trimmed) > 0 {
				var rec Record
				if jerr := json.Unmarshal(trimmed, &rec); jerr != nil {
					return out, fmt.Errorf("journal: corrupt record at line %d: %w", line, jerr)
				}
				out = append(out, rec)
			}
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("journal: %w", err)
		}
	}
}

// OrderEntry is the last known state of one order, rebuilt from the journal.
type OrderEntry struct {
	Key      Key
	PermID   int64
	Contract *ContractSnapshot
	Order    *OrderSnapshot

	// Status is the last status recorded (or "PendingSubmit" if only the
	// submit record exists).
	Status          string
	CancelRequested bool
	// Filled and AvgFillPrice are computed from unique fills.
	Filled       float64
	AvgFillPrice float64
	Fills        []FillSnapshot
	Commission   float64
	RealizedPNL  float64

	LastErrorCode int
	LastError     string
	Submitted     time.Time
	Updated       time.Time

	fillIDs map[string]bool
}

// IsDone reports whether the order reached a terminal status.
func (e *OrderEntry) IsDone() bool {
	return order.DoneStates[e.Status]
}

// Replay rebuilds per-order state from records (in journal order).
// Records that refer to an order by permId only are merged into the entry
// that carries the same permId.
func Replay(records []Record) map[Key]*OrderEntry {
	entries := make(map[Key]*OrderEntry)
	byPerm := make(map[int64]Key)

	get := func(r Record) *OrderEntry {
		k := r.KeyOf()
		if r.PermID != 0 {
			if pk, ok := byPerm[r.PermID]; ok {
				k = pk
			}
		}
		if k == (Key{}) {
			return nil
		}
		e := entries[k]
		if e == nil {
			e = &OrderEntry{Key: k, Status: order.StatusPendingSubmit, fillIDs: map[string]bool{}}
			entries[k] = e
		}
		if r.PermID != 0 {
			e.PermID = r.PermID
			byPerm[r.PermID] = k
		}
		if r.Time.After(e.Updated) {
			e.Updated = r.Time
		}
		return e
	}

	for _, r := range records {
		e := get(r)
		if e == nil {
			continue
		}
		switch r.Type {
		case TypeSubmit, TypeModify:
			if r.Contract != nil {
				e.Contract = r.Contract
			}
			if r.Order != nil {
				e.Order = r.Order
			}
			if r.Type == TypeSubmit && e.Submitted.IsZero() {
				e.Submitted = r.Time
			}
		case TypeCancelRequest:
			e.CancelRequested = true
		case TypeStatus:
			if r.Status != nil {
				e.Status = r.Status.Status
			}
		case TypeFill:
			if r.Fill != nil && !e.fillIDs[r.Fill.ExecID] {
				e.fillIDs[r.Fill.ExecID] = true
				notional := e.AvgFillPrice*e.Filled + r.Fill.Price*r.Fill.Shares
				e.Filled += r.Fill.Shares
				if e.Filled != 0 {
					e.AvgFillPrice = notional / e.Filled
				}
				e.Fills = append(e.Fills, *r.Fill)
				if e.Contract == nil && r.Contract != nil {
					e.Contract = r.Contract
				}
			}
		case TypeCommission:
			if r.Commission != nil {
				e.Commission += r.Commission.Commission
				e.RealizedPNL += r.Commission.RealizedPNL
			}
		case TypeError:
			e.LastErrorCode = r.ErrorCode
			e.LastError = r.Message
		}
	}
	return entries
}

// OpenEntries returns the entries that are not done, ordered by key.
func OpenEntries(entries map[Key]*OrderEntry) []*OrderEntry {
	var out []*OrderEntry
	for _, e := range entries {
		if !e.IsDone() {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.ClientID != out[j].Key.ClientID {
			return out[i].Key.ClientID < out[j].Key.ClientID
		}
		return out[i].Key.OrderID < out[j].Key.OrderID
	})
	return out
}

// Discrepancy kinds reported by Reconcile.
const (
	// MissingAtBroker: the journal has an open order the broker doesn't know.
	MissingAtBroker = "missing_at_broker"
	// UnknownToJournal: the broker has an order of this client the journal
	// never recorded (placed outside this process, or journal lost).
	UnknownToJournal = "unknown_to_journal"
	// FillMismatch: filled quantity differs between journal and broker.
	FillMismatch = "fill_mismatch"
)

// Discrepancy is a difference between the journal and the broker's view.
type Discrepancy struct {
	Kind  string
	Key   Key
	Entry *OrderEntry  // nil for UnknownToJournal
	Trade *order.Trade // nil for MissingAtBroker
	Info  string
}

// Reconcile compares journal entries with the broker's trades (typically
// ib.Trades() after ib.SyncOrders). Only trades of clientID are considered
// for UnknownToJournal.
func Reconcile(entries map[Key]*OrderEntry, trades []*order.Trade, clientID int64) []Discrepancy {
	var out []Discrepancy
	byKey := make(map[Key]*order.Trade, len(trades))
	byPerm := make(map[int64]*order.Trade, len(trades))
	for _, t := range trades {
		k := Key{ClientID: t.Order.ClientID, OrderID: t.Order.OrderID}
		if t.Order.OrderID <= 0 {
			k = Key{ClientID: 0, OrderID: t.PermID()}
		}
		byKey[k] = t
		if p := t.PermID(); p != 0 {
			byPerm[p] = t
		}
	}

	matched := make(map[*order.Trade]bool)
	for _, e := range sortedEntries(entries) {
		t := byKey[e.Key]
		if t == nil && e.PermID != 0 {
			t = byPerm[e.PermID]
		}
		if t == nil {
			if !e.IsDone() {
				out = append(out, Discrepancy{Kind: MissingAtBroker, Key: e.Key, Entry: e,
					Info: fmt.Sprintf("journal status %s, broker has no such order", e.Status)})
			}
			continue
		}
		matched[t] = true
		if bf := t.FilledQty(); bf != e.Filled {
			out = append(out, Discrepancy{Kind: FillMismatch, Key: e.Key, Entry: e, Trade: t,
				Info: fmt.Sprintf("journal filled %g, broker filled %g", e.Filled, bf)})
		}
	}
	for _, t := range trades {
		if matched[t] || t.Order.ClientID != clientID || t.Order.OrderID <= 0 {
			continue
		}
		out = append(out, Discrepancy{Kind: UnknownToJournal, Key: Key{ClientID: t.Order.ClientID, OrderID: t.Order.OrderID}, Trade: t,
			Info: "order not present in journal"})
	}
	return out
}

func sortedEntries(entries map[Key]*OrderEntry) []*OrderEntry {
	out := make([]*OrderEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.ClientID != out[j].Key.ClientID {
			return out[i].Key.ClientID < out[j].Key.ClientID
		}
		return out[i].Key.OrderID < out[j].Key.OrderID
	})
	return out
}
