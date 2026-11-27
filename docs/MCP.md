# MCP server for AI agents

`cmd/ib-mcp` is a [Model Context Protocol](https://modelcontextprotocol.io)
server that lets an AI agent (Claude Code, Claude Desktop, or any MCP client)
use Interactive Brokers through this library. It runs as a local stdio
process next to TWS or IB Gateway. The tools live in the `mcpserver`
package, which you can also embed in your own program.

```
MCP client (agent)  --stdio JSON-RPC-->  ib-mcp  --TWS API-->  TWS / IB Gateway
                                           |
                                           +-- mcpserver: tools, policy, previews
                                           +-- risk.Manager: pre-trade limits, kill switch
                                           +-- journal: write-ahead order log
```

## Quick start

```bash
go install github.com/kevinzhao-dev/go-ib-async/cmd/ib-mcp@latest
```

Read-only (market data, account, orders). Nothing can be traded:

```bash
claude mcp add ib -- ib-mcp -port 4002
```

Paper trading with limits:

```bash
claude mcp add ib -- ib-mcp -port 4002 -enable-trading \
  -journal ~/.ib-mcp/orders.jsonl -max-order-qty 100 -max-order-notional 20000
```

Claude Desktop (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "ib": {
      "command": "/Users/you/go/bin/ib-mcp",
      "args": ["-port", "4002"]
    }
  }
}
```

Ports: 4002 Gateway paper, 4001 Gateway live, 7497 TWS paper, 7496 TWS live.
The API must be enabled in TWS/Gateway (Configure → API → Settings). The
server starts even when the gateway is down and keeps reconnecting;
`get_status` tells the agent whether it is connected.

## Tools

| Tool | Kind | What it does |
|------|------|--------------|
| `get_status` | read | Connection, accounts (paper/live), market data type, trading state and risk limits |
| `search_contracts` | read | Contract lookup → `con_id`, names, min tick, exchanges, trading hours |
| `get_quote` | read | One-off snapshot: bid/ask/last/sizes/OHLC/volume, live or delayed |
| `get_historical_bars` | read | OHLCV bars (duration, bar size, data type, RTH) |
| `get_option_chain` | read | Expirations and strikes per exchange and trading class |
| `get_positions` | read | Positions with average cost |
| `get_account_summary` | read | NetLiquidation, cash, buying power, margin, … |
| `list_orders` / `get_order` | read | Order status, fills, commissions and event log (incl. rejection reasons) |
| `get_executions` | read | Today's fills with commission and realized P&L |
| `preview_order` | read | Validate an order, quote it, what-if margin/commission; returns a `preview_id` |
| `place_order` | trade | Send exactly the previewed order (`preview_id`) |
| `cancel_order` / `cancel_all_orders` | trade | Cancel orders of this API client |
| `halt_trading` | trade | Kill switch (optionally cancelling open orders); agents cannot undo it |

The trade tools are only registered with `-enable-trading`. Tools carry MCP
annotations (`readOnlyHint`, `destructiveHint`) so clients can auto-approve
read tools and ask the user before `place_order` and the cancels.

Instruments are described by a `contract` object (`symbol`, `sec_type`,
`exchange`, `currency`, `expiry`, `strike`, `right`, …, or just `con_id`).
Defaults: `STK`, `SMART`, `USD`; `EURUSD` with `CASH` becomes EUR.USD on
IDEALPRO. Tools that act on one instrument resolve it first and refuse
ambiguous matches, listing the candidates so the agent can retry with a
`con_id`.

## Order workflow

```
agent: preview_order {contract, action, quantity, order_type, limit_price, ...}
  server: validate → resolve contract → check account/policy → quote → what-if
  <- preview_id (single use, expires after -preview-ttl), summary, notional,
     margin impact, warnings ("will be rejected by max_order_quantity", ...)
agent: shows the preview to the user, who confirms
agent: place_order {preview_id}
  server: take preview (once) → PlaceOrder / PlaceBracketOrder
          → risk.Manager.CheckOrder → journal write-ahead → TWS
  <- order ids, status after the broker's acknowledgement
```

`preview_order` also covers brackets (`take_profit_price` + `stop_loss_price`
on a LMT entry) and modifications (`modify_order_id` with a new `quantity`,
`limit_price`, `stop_price` or `tif`). `place_order` takes nothing but the
preview id, so the agent cannot change an order between the preview the user
saw and what is sent.

## Safety model

Each layer works on its own; none relies on the model following its
instructions.

1. **Read-only by default.** Without `-enable-trading` the trade tools do
   not exist and `preview_order` never issues a preview id.
2. **Paper accounts only** unless `-allow-live`. Paper account ids start with
   `DU`/`DF`; anything else counts as live. Checked at preview and again at
   placement.
3. **Trading needs limits and a journal.** `-enable-trading` refuses to start
   without `-journal`, `-max-order-qty` and `-max-order-notional`. Other
   limits have conservative defaults: `-allowed-sec-types STK`,
   `-max-open-orders 10`, `-max-orders-per-minute 10`,
   `-price-collar-pct 5`, `-duplicate-window 30s`. The `risk` package
   enforces them inside `PlaceOrder` and fails closed (no reference price for
   a market order means it is rejected).
4. **Order type allowlist.** LMT, STP and STP LMT; MKT only with
   `-allow-market-orders`.
5. **Two-step orders.** Previews are single-use, expire (default 2 minutes)
   and are bound to the exact contract, side, quantity, prices, account and
   time in force.
6. **Write-ahead journal.** Every submit is fsync'd to the JSONL journal
   before it is sent; a journal failure blocks the order.
7. **Scope.** Agents can modify or cancel only orders of the server's API
   client id (`-client-id`, default 17), never other clients' or manual TWS
   orders, and never use global cancel. Orders are tagged
   `orderRef=ib-mcp` so they are recognisable in TWS.
8. **One-way kill switch.** `halt_trading` (and the daily loss limit) stop
   all new orders and modifications; there is no resume tool. Limits cannot
   be changed through MCP.
9. **Explicit instructions.** The server's MCP `instructions` tell the agent
   to preview first, get explicit user confirmation, treat risk rejections as
   final (no splitting or resizing), and halt when in doubt. Tool results
   (including text from IB such as contract names and error messages) are
   data for the agent, not instructions.

## Configuration

`ib-mcp -h` lists every flag. Connection flags also read `IB_HOST`, `IB_PORT`
and `IB_CLIENT_ID`. `-market-data-type 3` asks TWS for delayed data where the
account has no live subscription (quotes report `market_data_type`). Logs go
to stderr; stdout carries the protocol.

Embedding instead of the binary:

```go
rm := risk.New(limits, ib, risk.WithPriceSource(func(c *contract.Contract) (float64, bool) {
	return srv.ReferencePrice(c) // quotes the server has seen
}))
ib.SetRiskManager(rm)
ib.SetJournal(j)
srv, err := mcpserver.New(ib, mcpserver.Config{
	Trading: mcpserver.TradingConfig{Enabled: true, Risk: rm},
})
err = srv.Serve(ctx, os.Stdin, os.Stdout)
```

## Design notes

- **No dependencies.** The module has none, and a trading library should
  keep its supply chain small, so the stdio transport (JSON-RPC 2.0, one
  message per line) is implemented in `mcpserver/protocol.go`: `initialize`
  with version negotiation (2025-06-18, 2025-03-26, 2024-11-05), `ping`,
  `tools/list`, `tools/call`, and `notifications/cancelled`. Requests are
  handled concurrently. Moving to the official Go SDK later only touches
  that file.
- **Schemas from struct tags.** Tool arguments are Go structs; `json`,
  `desc`, `enum` and `required` tags generate the input schema and drive
  validation, so the two cannot drift. Unknown fields are rejected, and enums
  are matched case-insensitively.
- **Results** are JSON objects, sent as text content and as
  `structuredContent`, with snake_case keys and unset IB values (NaN,
  `UNSET_DOUBLE`, -1 prices) omitted. Failures are tool errors
  (`isError: true`) that the model can read and act on; protocol errors are
  only for malformed requests.
- **Quotes** use the new `IB.ReqTickerSnapshot`, which waits for
  `tickSnapshotEnd` and returns a ticker the reader goroutine no longer
  writes, so it can be read without a data race. The server caches the last
  price per contract as the risk manager's reference price.

## Limitations

- Snapshots only: no streaming subscriptions, MCP resources or prompts.
- The halt state and today's realized P&L live in memory; a restart clears
  them (a human restart is also how trading resumes).
- What-if is not available for modifications, and a bracket's what-if only
  covers the entry.
- The price collar and market-order notional use the last quote the server
  saw (at most 60s old); with delayed data that quote can be 15 minutes old.
- Order types beyond LMT/MKT/STP/STP LMT (trailing, algos, conditions,
  combos) are not exposed.
- Elicitation (asking the user for confirmation from the server side) is not
  used; confirmation relies on the client's tool approval and the agent's
  instructions.
