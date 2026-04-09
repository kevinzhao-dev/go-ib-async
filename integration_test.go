//go:build integration

// Integration tests require a live IB Gateway / TWS connection.
//
// Run with:
//   go test -tags=integration -v -run TestIntegration .
//
// Environment variables:
//   IB_HOST       (default: 127.0.0.1)
//   IB_PORT       (default: 4002)
//   IB_CLIENT_ID  (default: 99)
package ibgo

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
)

func getTestConfig() (string, int, int) {
	host := os.Getenv("IB_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := 4002
	if p := os.Getenv("IB_PORT"); p != "" {
		port, _ = strconv.Atoi(p)
	}
	clientID := 99
	if c := os.Getenv("IB_CLIENT_ID"); c != "" {
		clientID, _ = strconv.Atoi(c)
	}
	return host, port, clientID
}

func TestIntegrationConnect(t *testing.T) {
	host, port, clientID := getTestConfig()

	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ib.Connect(ctx, host, port, clientID); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer ib.Disconnect()

	if !ib.IsConnected() {
		t.Fatal("should be connected")
	}
	t.Logf("ServerVersion=%d, Accounts=%v", ib.ServerVersion(), ib.ManagedAccounts())
}

func TestIntegrationContractDetails(t *testing.T) {
	host, port, clientID := getTestConfig()

	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ib.Connect(ctx, host, port, clientID); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer ib.Disconnect()

	details, err := ib.ReqContractDetails(ctx, contract.Stock("AAPL", "SMART", "USD"))
	if err != nil {
		t.Fatalf("ReqContractDetails: %v", err)
	}
	if len(details) == 0 {
		t.Fatal("expected at least 1 contract detail")
	}
	if details[0].Contract.ConID != 265598 {
		t.Fatalf("AAPL conId = %d, want 265598", details[0].Contract.ConID)
	}
	t.Logf("AAPL: %s (conId=%d)", details[0].LongName, details[0].Contract.ConID)
}

func TestIntegrationHistoricalData(t *testing.T) {
	host, port, clientID := getTestConfig()

	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := ib.Connect(ctx, host, port, clientID); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer ib.Disconnect()

	con := contract.Stock("AAPL", "SMART", "USD")
	con.ConID = 265598

	bars, err := ib.ReqHistoricalData(ctx, con, "", "5 D", "1 day", "TRADES", true, 1)
	if err != nil {
		t.Fatalf("ReqHistoricalData: %v", err)
	}
	if len(bars) == 0 {
		t.Fatal("expected bars")
	}
	for _, b := range bars {
		t.Logf("%s O=%.2f H=%.2f L=%.2f C=%.2f V=%.0f",
			b.Date.Format("2006-01-02"), b.Open, b.High, b.Low, b.Close, b.Volume)
	}
}
