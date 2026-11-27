# Testing against a live account

Tools for checking the library against a real TWS / IB Gateway without
risking orders.

## Read-only API mode

Keep "Read-Only API" enabled in TWS/Gateway while developing against a live
account. Orders, `ReqOpenOrders` and `ReqCompletedOrders` are then rejected
(the errors match `ErrReadOnlyAPI`; `SyncOrders` falls back to
`ReqAllOpenOrders`).

## ibguard: an order-blocking proxy

`cmd/ibguard` sits between the client and the gateway and blocks every order
message (place, cancel, global cancel, exercise, ...) at the socket level, as
well as `reqOpenOrders` from client id 0, which binds manual TWS orders to the
API (TWS may then cancel and resubmit them). It fails closed: only message ids
on an explicit allowlist pass, and anything else ends the session. It caps the
negotiated API version at 178, above which messages can no longer be
inspected as text.

```bash
go run ./cmd/ibguard -upstream 127.0.0.1:4001 -listen 127.0.0.1:4011 &
```

## smoketest: read-only checks

`cmd/smoketest` runs ~45 read-only checks and sanity-checks the decoded
values. Account ids are masked in the output.

```bash
go run ./cmd/smoketest -port 4011 -guard-pid $(pgrep -x ibguard)
```

`-guard-pid` enables the reconnect check (the guard drops the connection on
SIGUSR1). Pass the guard's own pid, which it logs at startup; under `go run`,
`$!` is the `go` command, which does not forward the signal.

## mdcheck: market data permissions

`cmd/mdcheck` shows which market data the account receives through the API.
It requests live data (no delayed fallback) for US stocks on each network,
OPRA options, CME/CBOT/NYMEX/COMEX/CFE futures and the SPX index, plus
tick-by-tick, 5-second bars and market depth, and prints a verdict per line
(`OK live quotes`, `OK no quotes (closed?)`, `NOT SUBSCRIBED`, `DELAYED`, ...):

```bash
go run ./cmd/mdcheck -port 4011
```

Run it during US market hours to see quotes: outside them stocks and options
have no live quotes and stock tick-by-tick / 5-second bars stay empty, without
any permission error.

## Integration tests

```bash
make test-live        # read-only integration tests (IB_PORT, default 4002)
```

### Paper trading test

Order paths are tested end to end against `internal/fakegw`, an in-process
TWS fake that speaks the v178 wire protocol (layouts follow ib_async). The
paper-account trading test (`integration_trading_test.go`) places real paper
orders and must be run by you before trading live:

```bash
IB_E2E_TRADING=1 go test -tags=integration -v -run TestIntegrationTrading .
```
