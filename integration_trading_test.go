//go:build integration

// Live end-to-end trading test against a PAPER account.
//
// It places real (paper) orders, so it only runs when explicitly enabled:
//
//	IB_E2E_TRADING=1 go test -tags=integration -v -run TestIntegrationTrading .
//
// Safety: the test refuses to run unless every managed account starts with
// "DU" (IB paper accounts). The limit order is placed far below the market
// so it cannot fill, and it is cancelled before the test returns.
package ibgo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/journal"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

func TestIntegrationTradingE2E(t *testing.T) {
	if os.Getenv("IB_E2E_TRADING") != "1" {
		t.Skip("set IB_E2E_TRADING=1 to place paper orders")
	}
	host, port, clientID := getTestConfig()
	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := ib.Connect(ctx, host, port, clientID); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer ib.Disconnect()

	for _, a := range ib.ManagedAccounts() {
		if !strings.HasPrefix(a, "DU") {
			t.Fatalf("refusing to trade: account %s is not a paper account", a)
		}
	}

	j, err := journal.Open(filepath.Join(t.TempDir(), "orders.jsonl"), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ib.SetJournal(j)
	rm := risk.New(risk.Limits{MaxOrderQuantity: 1, MaxOrderNotional: 1000, MaxOpenOrders: 5}, ib)
	ib.SetRiskManager(rm)

	if _, err := ib.SyncOrders(ctx, SyncOptions{}); err != nil {
		t.Fatalf("SyncOrders: %v", err)
	}

	details, err := ib.ReqContractDetails(ctx, contract.Stock("AAPL", "SMART", "USD"))
	if err != nil || len(details) == 0 {
		t.Fatalf("ReqContractDetails: %v (%d)", err, len(details))
	}
	con := details[0].Contract

	// What-if: margin impact without placing.
	st, err := ib.WhatIfOrder(ctx, con, order.LimitOrder("BUY", 1, 1.00))
	if err != nil {
		t.Fatalf("WhatIfOrder: %v", err)
	}
	t.Logf("what-if: initMarginChange=%s commission=%g %s", st.InitMarginChange, st.Commission, st.CommissionCurrency)

	// Risk manager blocks an oversize order before it is sent.
	if _, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 2, 1.00)); err == nil {
		t.Fatal("risk manager should have blocked quantity 2")
	}

	// Place a limit far below the market, see it working, then cancel it.
	o := order.LimitOrder("BUY", 1, 1.00)
	o.TIF = "DAY"
	tr, err := ib.PlaceOrder(con, o)
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	defer ib.CancelOrder(tr.Order) // best effort if the test fails midway
	deadline := time.Now().Add(20 * time.Second)
	for !tr.IsWorking() && !tr.IsDone() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if tr.IsDone() {
		t.Fatalf("order ended early: %s %+v", tr.Status(), tr.LogSnapshot())
	}
	if !tr.IsWorking() && tr.Status() != order.StatusPreSubmitted {
		t.Fatalf("order not working: %s", tr.Status())
	}

	open, err := ib.ReqOpenOrders(ctx)
	if err != nil {
		t.Fatalf("ReqOpenOrders: %v", err)
	}
	found := false
	for _, x := range open {
		found = found || x == tr
	}
	if !found {
		t.Fatal("placed order missing from ReqOpenOrders")
	}

	cancelled := tr.CancelledEvent.Once()
	if _, err := ib.CancelOrder(tr.Order); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(20 * time.Second):
		t.Fatalf("cancel not confirmed: %s", tr.Status())
	}

	recs, err := journal.Load(j.Path())
	if err != nil {
		t.Fatal(err)
	}
	e := journal.Replay(recs)[journal.Key{ClientID: int64(clientID), OrderID: tr.Order.OrderID}]
	if e == nil || e.Status != order.StatusCancelled || !e.CancelRequested {
		t.Fatalf("journal entry = %+v", e)
	}
}
