// Smoke test against a live TWS/Gateway.
// Usage: go run ./cmd/smoketest
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
)

func main() {
	host := envOr("IB_HOST", "127.0.0.1")
	port := envIntOr("IB_PORT", 4002)
	clientID := envIntOr("IB_CLIENT_ID", 99)

	fmt.Printf("=== ibgo smoke test ===\n")
	fmt.Printf("Connecting to %s:%d ...\n", host, port)

	ib := ibgo.New()

	// Subscribe to errors
	ib.ErrorEvent.Subscribe(func(e *ibgo.IBError) {
		fmt.Printf("  [ERROR] code=%d reqId=%d: %s\n", e.Code, e.ReqID, e.Message)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := ib.Connect(ctx, host, port, clientID)
	if err != nil {
		fmt.Printf("FAIL: Connect error: %v\n", err)
		os.Exit(1)
	}
	defer ib.Disconnect()

	fmt.Printf("OK: Connected! ServerVersion=%d\n", ib.ServerVersion())
	fmt.Printf("OK: Accounts=%v\n", ib.ManagedAccounts())

	// Test 1: Request positions
	fmt.Println("\n--- 1. ReqPositions ---")
	positions, err := reqWithTimeout(func(ctx context.Context) (interface{}, error) {
		return ib.ReqPositions(ctx)
	})
	if err != nil {
		fmt.Printf("FAIL: %v\n", err)
	} else {
		pos := positions.([]account.Position)
		fmt.Printf("OK: %d positions\n", len(pos))
		for i, p := range pos {
			fmt.Printf("  [%d] %s %s: %.0f @ %.2f\n", i, p.Contract.Symbol, p.Contract.SecType, p.Position, p.AvgCost)
		}
	}

	// Test 2: Request account summary
	fmt.Println("\n--- 2. ReqAccountSummary ---")
	values, err := reqWithTimeout(func(ctx context.Context) (interface{}, error) {
		return ib.ReqAccountSummary(ctx, "All", "NetLiquidation,TotalCashValue,BuyingPower")
	})
	if err != nil {
		fmt.Printf("FAIL: %v\n", err)
	} else {
		avs := values.([]account.AccountValue)
		fmt.Printf("OK: %d values\n", len(avs))
		for _, v := range avs {
			fmt.Printf("  %s: %s = %s %s\n", v.Account, v.Tag, v.Value, v.Currency)
		}
	}

	// Test 3: Request contract details for AAPL
	fmt.Println("\n--- 3. ReqContractDetails (AAPL) ---")
	aaplContract := contract.Stock("AAPL", "SMART", "USD")
	details, err := reqWithTimeout(func(ctx context.Context) (interface{}, error) {
		return ib.ReqContractDetails(ctx, aaplContract)
	})
	if err != nil {
		fmt.Printf("FAIL: %v\n", err)
	} else {
		cds := details.([]contract.ContractDetails)
		fmt.Printf("OK: %d contract details\n", len(cds))
		for _, cd := range cds {
			c := cd.Contract
			fmt.Printf("  %s (conId=%d) %s | %s | minTick=%.4f\n",
				c.Symbol, c.ConID, c.PrimaryExchange, cd.LongName, cd.MinTick)
		}
	}

	// Test 4: Request historical data for AAPL
	fmt.Println("\n--- 4. ReqHistoricalData (AAPL, 5 days, 1 day bars) ---")
	// Need a qualified contract (with conId) for historical data
	if details != nil {
		cds := details.([]contract.ContractDetails)
		if len(cds) > 0 {
			qualifiedContract := cds[0].Contract
			bars, err := reqWithTimeout(func(ctx context.Context) (interface{}, error) {
				return ib.ReqHistoricalData(ctx, qualifiedContract, "", "5 D", "1 day", "TRADES", true, 1)
			})
			if err != nil {
				fmt.Printf("FAIL: %v\n", err)
			} else {
				barData := bars.([]market.BarData)
				fmt.Printf("OK: %d bars\n", len(barData))
				for _, b := range barData {
					fmt.Printf("  %s  O=%.2f H=%.2f L=%.2f C=%.2f V=%.0f\n",
						b.Date.Format("2006-01-02"), b.Open, b.High, b.Low, b.Close, b.Volume)
				}
			}
		}
	}

	// Test 5: Request market data snapshot for AAPL
	fmt.Println("\n--- 5. ReqMktData snapshot (AAPL) ---")
	if details != nil {
		cds := details.([]contract.ContractDetails)
		if len(cds) > 0 {
			qualifiedContract := cds[0].Contract
			ticker, err := ib.ReqMktData(qualifiedContract, "", true, false)
			if err != nil {
				fmt.Printf("FAIL: %v\n", err)
			} else {
				// Wait a moment for snapshot data to arrive
				time.Sleep(2 * time.Second)
				fmt.Printf("OK: bid=%.2f ask=%.2f last=%.2f vol=%.0f\n",
					ticker.Bid, ticker.Ask, ticker.Last, ticker.Volume)
			}
		}
	}

	// Test 6: Request option chain for AAPL
	fmt.Println("\n--- 6. ReqSecDefOptParams (AAPL) ---")
	if details != nil {
		cds := details.([]contract.ContractDetails)
		if len(cds) > 0 {
			chains, err := reqWithTimeout(func(ctx context.Context) (interface{}, error) {
				return ib.ReqSecDefOptParams(ctx, "AAPL", "", "STK", cds[0].Contract.ConID)
			})
			if err != nil {
				fmt.Printf("FAIL: %v\n", err)
			} else {
				optChains := chains.([]market.OptionChain)
				fmt.Printf("OK: %d option chains\n", len(optChains))
				for _, ch := range optChains {
					fmt.Printf("  %s class=%s mult=%s expirations=%d strikes=%d\n",
						ch.Exchange, ch.TradingClass, ch.Multiplier,
						len(ch.Expirations), len(ch.Strikes))
				}
			}
		}
	}

	fmt.Println("\n=== All tests complete ===")
}

func reqWithTimeout(fn func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return fn(ctx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
