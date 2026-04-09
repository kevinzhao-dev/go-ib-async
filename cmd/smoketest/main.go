// Smoke test against a live TWS/Gateway.
// Usage: go run ./cmd/smoketest
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
)

func main() {
	host := "127.0.0.1"
	port := 4002 // IB Gateway paper trading default

	fmt.Printf("Connecting to %s:%d ...\n", host, port)

	ib := ibgo.New()

	// Subscribe to errors
	ib.ErrorEvent.Subscribe(func(e *ibgo.IBError) {
		fmt.Printf("  [ERROR] code=%d reqId=%d: %s\n", e.Code, e.ReqID, e.Message)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := ib.Connect(ctx, host, port, 99)
	if err != nil {
		fmt.Printf("FAIL: Connect error: %v\n", err)
		os.Exit(1)
	}
	defer ib.Disconnect()

	fmt.Printf("OK: Connected! ServerVersion=%d\n", ib.ServerVersion())
	fmt.Printf("OK: Accounts=%v\n", ib.ManagedAccounts())

	// Test 1: Request positions
	fmt.Println("\n--- ReqPositions ---")
	posCtx, posCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer posCancel()

	positions, err := ib.ReqPositions(posCtx)
	if err != nil {
		fmt.Printf("FAIL: ReqPositions: %v\n", err)
	} else {
		fmt.Printf("OK: %d positions\n", len(positions))
		for i, p := range positions {
			fmt.Printf("  [%d] %s %s: %.0f @ %.2f\n", i, p.Contract.Symbol, p.Contract.SecType, p.Position, p.AvgCost)
		}
	}

	// Test 2: Request account summary
	fmt.Println("\n--- ReqAccountSummary ---")
	acctCtx, acctCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer acctCancel()

	values, err := ib.ReqAccountSummary(acctCtx, "All", "NetLiquidation,TotalCashValue,BuyingPower")
	if err != nil {
		fmt.Printf("FAIL: ReqAccountSummary: %v\n", err)
	} else {
		fmt.Printf("OK: %d account values\n", len(values))
		for _, v := range values {
			fmt.Printf("  %s: %s = %s %s\n", v.Account, v.Tag, v.Value, v.Currency)
		}
	}

	fmt.Println("\nDone. Disconnecting.")
}
