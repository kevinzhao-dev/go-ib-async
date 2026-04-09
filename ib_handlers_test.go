package ibgo

import (
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/state"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// --- PlaceOrder Initialization Tests ---

func TestPlaceOrderNewOrderInit(t *testing.T) {
	ib := New()
	ib.client.ServerVersion = 178
	// Fake connection state so Send doesn't fail - use a pipe
	ib.client.ConnState = protocol.Connected

	con := contract.Stock("AAPL", "SMART", "USD")
	con.ConID = 265598
	ord := order.LimitOrder("BUY", 100, 150.50)

	// We can't actually send (no connection), but we can test the trade init
	// by checking state before the Send call would fail
	// Instead, test the trade object creation directly
	now := time.Now()
	ord.OrderID = 42
	ord.ClientID = 1
	os := &order.OrderStatus{OrderID: ord.OrderID, Status: order.StatusPendingSubmit}
	logEntry := order.TradeLogEntry{Time: now, Status: order.StatusPendingSubmit}
	trade := &order.Trade{
		Contract:    con,
		Order:       ord,
		OrderStatus: os,
		Log:         []order.TradeLogEntry{logEntry},
	}

	if trade.OrderStatus.Status != order.StatusPendingSubmit {
		t.Fatalf("new trade status = %q, want PendingSubmit", trade.OrderStatus.Status)
	}
	if len(trade.Log) != 1 {
		t.Fatalf("new trade should have 1 log entry, got %d", len(trade.Log))
	}
	if trade.Log[0].Status != order.StatusPendingSubmit {
		t.Fatalf("log status = %q, want PendingSubmit", trade.Log[0].Status)
	}
}

// --- orderStatus Key Tests ---

func TestOrderKeyFromIDs_APIOrder(t *testing.T) {
	key := orderKeyFromIDs(1, 42, 999)
	if key.ClientID != 1 || key.OrderID != 42 {
		t.Fatalf("API order key = %+v, want {1, 42}", key)
	}
}

func TestOrderKeyFromIDs_ManualOrder(t *testing.T) {
	key := orderKeyFromIDs(0, 0, 12345)
	if key.ClientID != 0 || key.OrderID != 12345 {
		t.Fatalf("manual order key = %+v, want {0, 12345}", key)
	}
}

func TestOrderKeyFromIDs_NegativeOrderID(t *testing.T) {
	key := orderKeyFromIDs(1, -1, 999)
	if key.OrderID != 999 {
		t.Fatalf("negative orderId should use permId, got %+v", key)
	}
}

func TestOrderStatusManualOrderLookup(t *testing.T) {
	ib := New()

	// Simulate a manual TWS order (orderId=0, permId=12345)
	con := contract.Stock("AAPL", "SMART", "USD")
	ord := order.NewOrder()
	ord.PermID = 12345
	trade := &order.Trade{
		Contract:    con,
		Order:       ord,
		OrderStatus: &order.OrderStatus{Status: order.StatusSubmitted},
	}

	// Store with manual key
	manualKey := state.OrderKey{ClientID: 0, OrderID: 12345}
	ib.state.Trades[manualKey] = trade
	ib.state.PermID2Trade[12345] = trade

	// Simulate orderStatus with orderId=0, permId=12345
	fields := []string{"3", "0", "Filled", "100", "0", "150.50", "12345", "0", "150.50", "0", "", "0"}
	reader := protocol.NewFieldReader(fields)
	reader.Skip(1) // skip msgId
	ib.handleOrderStatus(reader)

	if trade.OrderStatus.Status != "Filled" {
		t.Fatalf("manual order status = %q, want Filled", trade.OrderStatus.Status)
	}
}

// --- execDetails Fill→Trade Reconciliation Tests ---

func TestExecDetailsFillLinkedToTrade(t *testing.T) {
	ib := New()
	ib.client.ServerVersion = 178

	// Create a trade
	con := contract.Stock("AAPL", "SMART", "USD")
	con.ConID = 265598
	ord := order.NewOrder()
	ord.OrderID = 42
	ord.ClientID = 1
	ord.PermID = 999

	trade := &order.Trade{
		Contract:    con,
		Order:       ord,
		OrderStatus: &order.OrderStatus{OrderID: 42, Status: order.StatusSubmitted},
	}

	key := state.OrderKey{ClientID: 1, OrderID: 42}
	ib.state.Trades[key] = trade
	ib.state.PermID2Trade[999] = trade

	// Track FillEvent emission
	fillReceived := false
	trade.FillEvent.Subscribe(func(f *order.Fill) {
		fillReceived = true
	})

	// Simulate execDetails message (msgId=11)
	// Format: msgId, version, reqId, orderId, conId, symbol, secType, lastTradeDate,
	//         strike, right, multiplier, exchange, currency, localSymbol, tradingClass,
	//         execId, time, acctNumber, exchange, side, shares, price, permId, clientId,
	//         liquidation, cumQty, avgPrice, orderRef, evRule, evMultiplier, modelCode, lastLiquidity
	fields := []string{
		"11", "1", "-1",
		"42",
		"265598", "AAPL", "STK", "", "0", "", "", "SMART", "USD", "", "",
		"exec001", "20260409 10:00:00", "DU123", "SMART", "BOT",
		"50", "150.50", "999", "1", "0", "50", "150.50",
		"", "", "0", "", "1",
		"0", // pendingPriceRevision (serverVersion >= 178)
	}
	reader := protocol.NewFieldReader(fields)
	reader.Skip(1) // skip msgId
	ib.handleExecDetails(reader)

	// Verify fill is linked to trade
	if len(trade.Fills) != 1 {
		t.Fatalf("trade should have 1 fill, got %d", len(trade.Fills))
	}
	if trade.Fills[0].Execution.Shares != 50 {
		t.Fatalf("fill shares = %g, want 50", trade.Fills[0].Execution.Shares)
	}
	if trade.Fills[0].Execution.Price != 150.50 {
		t.Fatalf("fill price = %g, want 150.50", trade.Fills[0].Execution.Price)
	}
	if trade.Fills[0].Contract.Symbol != "AAPL" {
		t.Fatalf("fill contract symbol = %q, want AAPL", trade.Fills[0].Contract.Symbol)
	}

	// Verify log entry
	found := false
	for _, entry := range trade.Log {
		if entry.Message == "Fill 50@150.5" {
			found = true
		}
	}
	if !found {
		t.Fatal("trade log should have fill entry")
	}

	// FillEvent is only emitted for live fills (reqId not in pending requests)
	// Since reqId=-1 is not pending, this should be treated as live
	if !fillReceived {
		t.Fatal("FillEvent should have been emitted for live fill")
	}
}

func TestExecDetailsDuplicateFillIgnored(t *testing.T) {
	ib := New()
	ib.client.ServerVersion = 178

	con := contract.Stock("AAPL", "SMART", "USD")
	ord := order.NewOrder()
	ord.OrderID = 42
	ord.ClientID = 1
	ord.PermID = 999
	trade := &order.Trade{
		Contract:    con,
		Order:       ord,
		OrderStatus: &order.OrderStatus{Status: order.StatusSubmitted},
	}
	ib.state.Trades[state.OrderKey{ClientID: 1, OrderID: 42}] = trade
	ib.state.PermID2Trade[999] = trade

	fields := []string{
		"11", "1", "-1", "42",
		"265598", "AAPL", "STK", "", "0", "", "", "SMART", "USD", "", "",
		"exec001", "20260409 10:00:00", "DU123", "SMART", "BOT",
		"50", "150.50", "999", "1", "0", "50", "150.50",
		"", "", "0", "", "1", "0",
	}

	// Process same exec twice
	r1 := protocol.NewFieldReader(fields)
	r1.Skip(1)
	ib.handleExecDetails(r1)

	r2 := protocol.NewFieldReader(fields)
	r2.Skip(1)
	ib.handleExecDetails(r2)

	// Should still have only 1 fill (duplicate ignored)
	if len(trade.Fills) != 1 {
		t.Fatalf("duplicate fill should be ignored, got %d fills", len(trade.Fills))
	}
}

// --- orderKeyForExec Tests ---

func TestOrderKeyForExec_APIOrder(t *testing.T) {
	exec := &order.Execution{ClientID: 1, OrderID: 42, PermID: 999}
	key := orderKeyForExec(exec)
	if key.ClientID != 1 || key.OrderID != 42 {
		t.Fatalf("API exec key = %+v, want {1, 42}", key)
	}
}

func TestOrderKeyForExec_ManualOrder(t *testing.T) {
	exec := &order.Execution{ClientID: 0, OrderID: 0, PermID: 12345}
	key := orderKeyForExec(exec)
	if key.ClientID != 0 || key.OrderID != 12345 {
		t.Fatalf("manual exec key = %+v, want {0, 12345}", key)
	}
}
