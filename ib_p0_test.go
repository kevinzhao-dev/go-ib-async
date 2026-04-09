package ibgo

import (
	"context"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/state"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// --- Partial Fill Tests ---

func TestPartialFillAccumulation(t *testing.T) {
	ib := New()
	ib.client.ServerVersion = 178

	con := contract.Stock("AAPL", "SMART", "USD")
	con.ConID = 265598
	ord := order.NewOrder()
	ord.OrderID = 42
	ord.ClientID = 1
	ord.PermID = 999
	ord.TotalQuantity = 100

	trade := &order.Trade{
		Contract:    con,
		Order:       ord,
		OrderStatus: &order.OrderStatus{OrderID: 42, Status: order.StatusSubmitted},
	}
	ib.state.Trades[state.OrderKey{ClientID: 1, OrderID: 42}] = trade
	ib.state.PermID2Trade[999] = trade

	// First partial fill: 30 shares
	sendExecDetails(t, ib, "exec001", 30, 150.00, 999, 1, 42)

	if trade.FilledQty() != 30 {
		t.Fatalf("after first fill: FilledQty=%g, want 30", trade.FilledQty())
	}
	if trade.RemainingQty() != 70 {
		t.Fatalf("after first fill: RemainingQty=%g, want 70", trade.RemainingQty())
	}

	// Second partial fill: 50 shares
	sendExecDetails(t, ib, "exec002", 50, 150.25, 999, 1, 42)

	if trade.FilledQty() != 80 {
		t.Fatalf("after second fill: FilledQty=%g, want 80", trade.FilledQty())
	}
	if trade.RemainingQty() != 20 {
		t.Fatalf("after second fill: RemainingQty=%g, want 20", trade.RemainingQty())
	}

	// Final fill: 20 shares
	sendExecDetails(t, ib, "exec003", 20, 150.50, 999, 1, 42)

	if trade.FilledQty() != 100 {
		t.Fatalf("after final fill: FilledQty=%g, want 100", trade.FilledQty())
	}
	if trade.RemainingQty() != 0 {
		t.Fatalf("after final fill: RemainingQty=%g, want 0", trade.RemainingQty())
	}
	if len(trade.Fills) != 3 {
		t.Fatalf("expected 3 fills, got %d", len(trade.Fills))
	}
}

func sendExecDetails(t *testing.T, ib *IB, execID string, shares, price float64, permID, clientID, orderID int64) {
	t.Helper()
	fields := []string{
		"11", "1", "-1",
		itoa(orderID),
		"265598", "AAPL", "STK", "", "0", "", "", "SMART", "USD", "", "",
		execID, "20260409 10:00:00", "DU123", "SMART", "BOT",
		ftoa(shares), ftoa(price), itoa(permID), itoa(clientID), "0",
		ftoa(shares), ftoa(price),
		"", "", "0", "", "1", "0",
	}
	r := protocol.NewFieldReader(fields)
	r.Skip(1)
	ib.handleExecDetails(r)
}

// --- Status Transition Tests ---

func TestOrderStatusTransitions(t *testing.T) {
	ib := New()

	con := contract.Stock("AAPL", "SMART", "USD")
	ord := order.NewOrder()
	ord.OrderID = 42
	ord.ClientID = 1
	ord.PermID = 999

	trade := &order.Trade{
		Contract:    con,
		Order:       ord,
		OrderStatus: &order.OrderStatus{OrderID: 42, Status: order.StatusPendingSubmit},
	}
	ib.state.Trades[state.OrderKey{ClientID: 1, OrderID: 42}] = trade
	ib.state.PermID2Trade[999] = trade

	// Track events
	var statusHistory []string
	trade.StatusEvent.Subscribe(func(tr *order.Trade) {
		statusHistory = append(statusHistory, tr.OrderStatus.Status)
	})

	filledCount := 0
	trade.FilledEvent.Subscribe(func(tr *order.Trade) {
		filledCount++
	})

	cancelledCount := 0
	trade.CancelledEvent.Subscribe(func(tr *order.Trade) {
		cancelledCount++
	})

	// Transition: PendingSubmit → Submitted
	sendOrderStatus(ib, 42, "Submitted", 0, 100, 0, 999, 1)
	// Transition: Submitted → Filled
	sendOrderStatus(ib, 42, "Filled", 100, 0, 150.50, 999, 1)

	if len(statusHistory) != 2 {
		t.Fatalf("expected 2 status events, got %d: %v", len(statusHistory), statusHistory)
	}
	if statusHistory[0] != "Submitted" || statusHistory[1] != "Filled" {
		t.Fatalf("status history = %v, want [Submitted, Filled]", statusHistory)
	}
	if filledCount != 1 {
		t.Fatalf("FilledEvent should fire exactly once, got %d", filledCount)
	}
	if cancelledCount != 0 {
		t.Fatal("CancelledEvent should not fire for filled order")
	}
}

func TestOrderCancelledEvent(t *testing.T) {
	ib := New()

	ord := order.NewOrder()
	ord.OrderID = 42
	ord.ClientID = 1
	ord.PermID = 999

	trade := &order.Trade{
		Contract:    contract.Stock("AAPL", "SMART", "USD"),
		Order:       ord,
		OrderStatus: &order.OrderStatus{OrderID: 42, Status: order.StatusSubmitted},
	}
	ib.state.Trades[state.OrderKey{ClientID: 1, OrderID: 42}] = trade
	ib.state.PermID2Trade[999] = trade

	cancelled := false
	trade.CancelledEvent.Subscribe(func(*order.Trade) { cancelled = true })

	sendOrderStatus(ib, 42, "Cancelled", 0, 100, 0, 999, 1)

	if !cancelled {
		t.Fatal("CancelledEvent should fire")
	}
	if !trade.IsDone() {
		t.Fatal("Cancelled trade should be done")
	}
}

func sendOrderStatus(ib *IB, orderID int64, status string, filled, remaining, avgPrice float64, permID, clientID int64) {
	fields := []string{
		"3",
		itoa(orderID), status, ftoa(filled), ftoa(remaining), ftoa(avgPrice),
		itoa(permID), "0", ftoa(avgPrice), itoa(clientID), "", "0",
	}
	r := protocol.NewFieldReader(fields)
	r.Skip(1)
	ib.handleOrderStatus(r)
}

// --- Reconnection Tests ---

func TestConnectWithReconnect(t *testing.T) {
	var connectCount int64

	// Server that accepts 2 connections, drops first quickly
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	go func() {
		for i := 0; i < 2; i++ {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			// Handshake
			buf := make([]byte, 4)
			conn.Read(buf) // API\0
			conn.Read(buf) // length
			vLen := binary.BigEndian.Uint32(buf)
			vBuf := make([]byte, vLen)
			conn.Read(vBuf)
			sendMsg2(conn, "178\x00connTime\x00")

			// Read startApi
			conn.Read(buf)
			mLen := binary.BigEndian.Uint32(buf)
			mBuf := make([]byte, mLen)
			conn.Read(mBuf)

			sendMsg2(conn, "9\x001\x001\x00")
			sendMsg2(conn, "15\x001\x00DU123456\x00")

			if i == 0 {
				// First connection: drop after 100ms
				time.Sleep(100 * time.Millisecond)
				conn.Close()
			} else {
				// Second connection: stay alive
				time.Sleep(2 * time.Second)
				conn.Close()
			}
		}
	}()

	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := ReconnectConfig{
		RetryDelay: 200 * time.Millisecond,
		MaxRetries: 3,
	}

	go func() {
		ib.ConnectWithReconnect(ctx, "127.0.0.1", port, 1, cfg, func() {
			atomic.AddInt64(&connectCount, 1)
		})
	}()

	// Wait for at least one reconnection
	time.Sleep(3 * time.Second)
	cancel()

	count := atomic.LoadInt64(&connectCount)
	if count < 2 {
		t.Fatalf("expected at least 2 connections (initial + reconnect), got %d", count)
	}
}

func sendMsg2(conn net.Conn, msg string) {
	payload := []byte(msg)
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	conn.Write(header)
	conn.Write(payload)
}

// --- Helpers ---

func itoa(n int64) string {
	return protocol.EncodeField(int(n))
}

func ftoa(f float64) string {
	return protocol.EncodeField(f)
}
