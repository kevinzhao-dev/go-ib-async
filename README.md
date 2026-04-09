# go-ib-async

Go client for the Interactive Brokers TWS/Gateway API. Port of the Python [ib_async](https://github.com/ib-api-reloaded/ib_async) library.

> **Status: Core subset only. Not production-ready. Not full parity with ib_async.**
> See [STATUS.md](STATUS.md) for the complete parity matrix including what's implemented, partial, and missing.

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    ibgo "github.com/kevinzhao-dev/go-ib-async"
    "github.com/kevinzhao-dev/go-ib-async/contract"
)

func main() {
    ib := ibgo.New()
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    // Connect to IB Gateway (paper: 4002, live: 4001)
    if err := ib.Connect(ctx, "127.0.0.1", 4002, 1); err != nil {
        log.Fatal(err)
    }
    defer ib.Disconnect()

    // Request contract details
    details, err := ib.ReqContractDetails(ctx, contract.Stock("AAPL", "SMART", "USD"))
    if err != nil {
        log.Fatal(err)
    }
    if len(details) == 0 {
        log.Fatal("no contract details returned")
    }
    fmt.Printf("AAPL conId=%d\n", details[0].Contract.ConID)

    // Request historical data
    bars, err := ib.ReqHistoricalData(ctx, details[0].Contract,
        "", "5 D", "1 day", "TRADES", true, 1)
    if err != nil {
        log.Fatal(err)
    }
    for _, b := range bars {
        fmt.Printf("%s  O=%.2f H=%.2f L=%.2f C=%.2f\n",
            b.Date.Format("2006-01-02"), b.Open, b.High, b.Low, b.Close)
    }

    // Market data snapshot (idiomatic: use event, not sleep)
    ticker, err := ib.ReqMktData(details[0].Contract, "", true, false)
    if err != nil {
        log.Fatal(err)
    }
    ch := ticker.UpdateEvent.Once()
    <-ch // wait for first tick update
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

10 methods live-verified against IB Gateway. 29/82 inbound message types handled. See [STATUS.md](STATUS.md) for full details.

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

**Concurrency model:** Single reader goroutine processes all inbound messages. Request-response matching via per-request buffered channels. State mutations on the reader goroutine, thread-safe reads via `sync.RWMutex`.

## Development

```bash
go build ./...                                          # build
go test -race ./...                                     # test
go test -bench=. -benchmem ./protocol/                  # benchmarks
go test -fuzz=FuzzDecodeMessage -fuzztime=10s ./protocol/ # fuzz
go test -cover ./...                                    # coverage
go run ./cmd/smoketest                                  # live test (needs Gateway)
```

## Known Limitations

- **Not production-ready for live trading.** Order execution edge cases (partial fills, status transitions, commission timing) need more coverage.
- **No reconnection / heartbeat.** IB Gateway disconnects are routine; this library does not auto-reconnect.
- **35% message coverage** (29/82 inbound types). Many advanced features are not yet implemented.
- **No request throttling** at runtime (constants defined, enforcement not implemented).

## Requirements

- Go 1.24+
- IB TWS or Gateway (for live testing)

## License

BSD 2-Clause. See [LICENSE](LICENSE).
