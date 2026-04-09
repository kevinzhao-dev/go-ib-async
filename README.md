# go-ib-async

Go client for the Interactive Brokers TWS/Gateway API. Port of the Python [ib_async](https://github.com/ib-api-reloaded/ib_async) library.

> **Status: Core subset.** See [STATUS.md](STATUS.md) for the full parity matrix.

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "time"

    ibgo "github.com/kevinzhao-dev/go-ib-async"
    "github.com/kevinzhao-dev/go-ib-async/contract"
)

func main() {
    ib := ibgo.New()
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    // Connect to IB Gateway (paper: 4002, live: 4001)
    err := ib.Connect(ctx, "127.0.0.1", 4002, 1)
    if err != nil {
        panic(err)
    }
    defer ib.Disconnect()

    // Request contract details
    details, _ := ib.ReqContractDetails(ctx, contract.Stock("AAPL", "SMART", "USD"))
    fmt.Printf("AAPL conId=%d\n", details[0].Contract.ConID)

    // Request historical data
    bars, _ := ib.ReqHistoricalData(ctx, details[0].Contract,
        "", "5 D", "1 day", "TRADES", true, 1)
    for _, b := range bars {
        fmt.Printf("%s  O=%.2f H=%.2f L=%.2f C=%.2f\n",
            b.Date.Format("2006-01-02"), b.Open, b.High, b.Low, b.Close)
    }

    // Market data snapshot
    ticker, _ := ib.ReqMktData(details[0].Contract, "", true, false)
    time.Sleep(2 * time.Second)
    fmt.Printf("bid=%.2f ask=%.2f last=%.2f\n", ticker.Bid, ticker.Ask, ticker.Last)
}
```

## What Works

| Feature | Status |
|---------|--------|
| Connect / Disconnect | Verified |
| Contract Details | Verified |
| Historical Data (bars) | Verified |
| Market Data (snapshot) | Verified |
| Positions | Verified |
| Account Summary | Verified |
| Option Chains | Verified |
| Place / Cancel Order | Implemented |
| Real-Time Bars | Implemented |

See [STATUS.md](STATUS.md) for full details on what's implemented, partial, and missing.

## Architecture

```
ibgo (root)          IB facade - Connect, public API, message dispatch
  contract/          Contract, Stock, Option, Future, etc.
  order/             Order, Trade, OrderStatus, conditions
  market/            Ticker, BarData, OptionComputation, depth
  account/           AccountValue, Position, PnL, news
  event/             Generic Event[T] pub/sub
  protocol/          TCP connection, wire encoder/decoder, client
  internal/state/    State manager, request tracking, tick maps
```

**Concurrency model:** Single reader goroutine processes all inbound messages. Request-response matching via per-request buffered channels. All state mutations on the reader goroutine. Thread-safe read accessors via `sync.RWMutex`.

## Development

```bash
# Build
go build ./...

# Test (with race detector)
go test -race ./...

# Benchmarks
go test -bench=. -benchmem ./protocol/

# Fuzz (10 seconds)
go test -fuzz=FuzzDecodeMessage -fuzztime=10s ./protocol/

# Coverage
go test -cover ./...

# Live smoke test (requires IB Gateway running)
go run ./cmd/smoketest
```

## Requirements

- Go 1.21+
- IB TWS or Gateway (for live testing)

## License

Same as upstream ib_async.
