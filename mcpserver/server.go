// Package mcpserver exposes an IB client to AI agents as a Model Context
// Protocol (MCP) server over stdio.
//
// Read-only tools (contracts, quotes, bars, option chains, positions,
// account, orders, executions, what-if previews) are always available.
// Trading tools are only registered when TradingConfig.Enabled is set, and
// every order goes through two steps: preview_order validates the order,
// resolves the contract, runs a what-if and returns a single-use preview id;
// place_order(preview_id) sends exactly the previewed order through the
// client's risk manager and journal. Agents can halt trading but never
// resume it.
//
//	srv, err := mcpserver.New(ib, mcpserver.Config{})
//	err = srv.Serve(ctx, os.Stdin, os.Stdout)
//
// See cmd/ib-mcp for the ready-made binary and docs/MCP.md for the design.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

// Broker is the part of *ibgo.IB the server uses.
type Broker interface {
	IsConnected() bool
	// LastConnectError is why the last connection attempt failed (nil when
	// it succeeded), see ibgo.IB.LastConnectError.
	LastConnectError() error
	ServerVersion() int
	ManagedAccounts() []string
	ClientID() int64
	ReqContractDetails(ctx context.Context, con *contract.Contract) ([]contract.ContractDetails, error)
	ReqTickerSnapshot(ctx context.Context, con *contract.Contract) (*market.Ticker, error)
	ReqHistoricalData(ctx context.Context, con *contract.Contract, endDateTime, durationStr, barSizeSetting, whatToShow string, useRTH bool, formatDate int) ([]market.BarData, error)
	ReqSecDefOptParams(ctx context.Context, underlyingSymbol, futFopExchange, underlyingSecType string, underlyingConID int64) ([]market.OptionChain, error)
	ReqPositions(ctx context.Context) ([]account.Position, error)
	ReqAccountSummary(ctx context.Context, groupName, tags string) ([]account.AccountValue, error)
	ReqExecutions(ctx context.Context, filter order.ExecutionFilter) ([]order.Fill, error)
	// OpenOrdersSnapshot returns every client's open orders without making
	// the client track other clients' orders (see ibgo.IB.OpenOrdersSnapshot).
	OpenOrdersSnapshot(ctx context.Context) ([]*order.Trade, error)
	Trades() []*order.Trade
	OpenTrades() []*order.Trade
	TradeFills(t *order.Trade) []order.Fill
	WhatIfOrder(ctx context.Context, con *contract.Contract, ord *order.Order) (*order.OrderState, error)
	PlaceOrder(con *contract.Contract, ord *order.Order) (*order.Trade, error)
	PlaceBracketOrder(con *contract.Contract, b *order.BracketOrder) ([]*order.Trade, error)
	CancelOrder(ord *order.Order) (*order.Trade, error)
	// RiskManager and Journal report the hooks PlaceOrder consults; trading
	// is refused unless they are TradingConfig.Risk and a journal.
	RiskManager() ibgo.PreTradeChecker
	Journal() ibgo.OrderJournal
}

var _ Broker = (*ibgo.IB)(nil)

// Config configures a Server. Zero values get the documented defaults.
type Config struct {
	// Name and Version are reported to the client (default "ib-mcp", "dev").
	Name    string
	Version string
	// RequestTimeout bounds every tool call (default 30s).
	RequestTimeout time.Duration
	// MaxBars caps the bars returned by get_historical_bars (default 2000).
	MaxBars int
	// QuoteMaxAge is how long a quote stays usable as the reference price
	// for risk checks (default 60s).
	QuoteMaxAge time.Duration
	// MarketDataType is reported by get_status (1 live, 3 delayed, ...);
	// the caller is responsible for requesting it (default 1).
	MarketDataType int
	// DisableWhatIf stops preview_order from asking TWS for the margin and
	// commission impact. That request is a placeOrder message with whatIf
	// set, which TWS evaluates but never executes; with DisableWhatIf a
	// read-only server sends no order messages at all.
	DisableWhatIf bool

	Trading TradingConfig
}

// TradingConfig controls the order tools.
type TradingConfig struct {
	// Enabled registers preview ids and the trading tools (place_order,
	// cancel_order, cancel_all_orders, halt_trading). Requires Risk.
	Enabled bool
	// AllowLive permits orders for accounts that are not paper accounts
	// (paper account ids start with DU or DF). Default: paper only.
	AllowLive bool
	// OrderTypes are the order types agents may use (default LMT, STP,
	// STP LMT). Add "MKT" to allow market orders.
	OrderTypes []string
	// PreviewTTL is how long a preview id can be placed (default 2m).
	PreviewTTL time.Duration
	// OrderRef is set on every order placed through the server, to tell
	// agent orders apart in TWS (default "ib-mcp").
	OrderRef string
	// Risk is the risk manager installed on the client with
	// SetRiskManager. The server reports its limits and halt state and
	// uses it for halt_trading; the checks themselves run in PlaceOrder.
	// The client must also have an order journal (SetJournal); New and
	// place_order refuse to trade otherwise.
	Risk *risk.Manager
	// Ready reports whether the client's orders and positions are
	// synchronised with the broker for the current connection (for example
	// after SyncOrders succeeded). place_order refuses to send while it
	// returns an error, so risk checks never run against stale state. Nil
	// means always ready.
	Ready func() error
}

// Server is an MCP server for one IB client.
type Server struct {
	b   Broker
	cfg Config

	tools      []*tool
	toolByName map[string]*tool

	previews *previewStore

	// placeMu is held (shared) while place_order submits, and exclusively
	// by halt_trading while it collects the orders to cancel, so an order
	// that passed the risk check before the halt is registered (and
	// cancelled) rather than missed.
	placeMu sync.RWMutex

	quoteMu sync.Mutex
	quotes  map[int64]quoteEntry

	detailsMu sync.Mutex
	details   map[int64]contract.ContractDetails
}

type quoteEntry struct {
	price float64
	at    time.Time
}

// Supported order types (TradingConfig.OrderTypes must be a subset).
var supportedOrderTypes = []string{"LMT", "MKT", "STP", "STP LMT"}

// New creates a Server for b.
func New(b Broker, cfg Config) (*Server, error) {
	if b == nil {
		return nil, errors.New("mcpserver: nil broker")
	}
	if cfg.Name == "" {
		cfg.Name = "ib-mcp"
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if cfg.MaxBars <= 0 {
		cfg.MaxBars = 2000
	}
	if cfg.QuoteMaxAge <= 0 {
		cfg.QuoteMaxAge = time.Minute
	}
	if cfg.MarketDataType == 0 {
		cfg.MarketDataType = 1
	}
	tc := &cfg.Trading
	if len(tc.OrderTypes) == 0 {
		tc.OrderTypes = []string{"LMT", "STP", "STP LMT"}
	}
	for i, ot := range tc.OrderTypes {
		ot = strings.ToUpper(strings.TrimSpace(ot))
		if !contains(supportedOrderTypes, ot) {
			return nil, fmt.Errorf("mcpserver: unsupported order type %q (supported: %s)", ot, strings.Join(supportedOrderTypes, ", "))
		}
		tc.OrderTypes[i] = ot
	}
	if tc.PreviewTTL <= 0 {
		tc.PreviewTTL = 2 * time.Minute
	}
	if tc.OrderRef == "" {
		tc.OrderRef = "ib-mcp"
	}
	if tc.Enabled {
		if tc.Risk == nil {
			return nil, errors.New("mcpserver: trading needs a risk manager (TradingConfig.Risk)")
		}
		if l := tc.Risk.Limits(); !finitePositive(l.MaxOrderQuantity) || !finitePositive(l.MaxOrderNotional) {
			return nil, errors.New("mcpserver: trading needs finite positive risk limits MaxOrderQuantity and MaxOrderNotional")
		}
	}

	s := &Server{
		b:          b,
		cfg:        cfg,
		toolByName: map[string]*tool{},
		previews:   newPreviewStore(),
		quotes:     map[int64]quoteEntry{},
		details:    map[int64]contract.ContractDetails{},
	}
	if tc.Enabled {
		if err := s.checkHooks(); err != nil {
			return nil, fmt.Errorf("mcpserver: %w", err)
		}
	}
	s.registerTools()
	return s, nil
}

// checkHooks verifies that the client's PlaceOrder consults this server's
// risk manager and a write-ahead journal.
func (s *Server) checkHooks() error {
	if rc := s.b.RiskManager(); rc == nil || rc != ibgo.PreTradeChecker(s.cfg.Trading.Risk) {
		return errors.New("trading needs TradingConfig.Risk installed on the client (SetRiskManager)")
	}
	if s.b.Journal() == nil {
		return errors.New("trading needs an order journal installed on the client (SetJournal)")
	}
	return nil
}

// tradingReady reports why orders cannot be sent yet (TradingConfig.Ready).
func (s *Server) tradingReady() error {
	if s.cfg.Trading.Ready == nil {
		return nil
	}
	if err := s.cfg.Trading.Ready(); err != nil {
		return fmt.Errorf("broker state is not synchronised yet (%v); nothing was sent, try again shortly", err)
	}
	return nil
}

func finitePositive(v float64) bool { return v > 0 && !math.IsInf(v, 1) }

// ReferencePrice returns the last quoted price of con seen by the server
// (midpoint, else last, else close) if it is younger than QuoteMaxAge. Pass
// it to the risk manager with risk.WithPriceSource so market-order notional
// and price-collar checks have a reference.
func (s *Server) ReferencePrice(con *contract.Contract) (float64, bool) {
	return s.referenceWithin(con, s.cfg.QuoteMaxAge)
}

// referenceWithin returns the last quoted price of con if it is younger
// than maxAge.
func (s *Server) referenceWithin(con *contract.Contract, maxAge time.Duration) (float64, bool) {
	if con == nil || con.ConID == 0 {
		return 0, false
	}
	s.quoteMu.Lock()
	defer s.quoteMu.Unlock()
	q, ok := s.quotes[con.ConID]
	if !ok || time.Since(q.at) > maxAge {
		return 0, false
	}
	return q.price, true
}

func (s *Server) rememberQuote(t *market.Ticker) {
	if t == nil || t.Contract == nil || t.Contract.ConID == 0 {
		return
	}
	px, ok := referenceOf(t)
	if !ok {
		return
	}
	s.quoteMu.Lock()
	s.quotes[t.Contract.ConID] = quoteEntry{price: px, at: time.Now()}
	s.quoteMu.Unlock()
}

func referenceOf(t *market.Ticker) (float64, bool) {
	bid, ask := price(t.Bid), price(t.Ask)
	if bid != nil && ask != nil && *bid > 0 && *ask >= *bid {
		return (*bid + *ask) / 2, true
	}
	for _, v := range []float64{t.Last, t.Close} {
		if p := price(v); p != nil && *p > 0 {
			return *p, true
		}
	}
	return 0, false
}

// --- tools ---

type toolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

type tool struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description"`
	InputSchema map[string]any   `json:"inputSchema"`
	Annotations *toolAnnotations `json:"annotations,omitempty"`

	handler func(ctx context.Context, args json.RawMessage) (any, error)
}

var (
	readOnly    = toolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true}
	destructive = toolAnnotations{DestructiveHint: true, OpenWorldHint: true}
)

// addTool registers a tool whose arguments decode into A.
func addTool[A any](s *Server, name, title, desc string, ann toolAnnotations, fn func(ctx context.Context, a A) (any, error)) {
	ann.Title = title
	t := &tool{
		Name:        name,
		Title:       title,
		Description: desc,
		InputSchema: schemaFor(reflect.TypeOf((*A)(nil)).Elem()),
		Annotations: &ann,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a A
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			return fn(ctx, a)
		},
	}
	s.tools = append(s.tools, t)
	s.toolByName[name] = t
}

func (s *Server) registerTools() {
	addTool(s, "get_status", "Connection and trading status",
		"Connection state, managed accounts (paper or live), market data type, and whether trading is enabled, halted, and under which risk limits. Call this first.",
		readOnly, s.getStatus)
	addTool(s, "search_contracts", "Search contracts",
		"Look up instruments matching a contract description and return their IB contract ids (con_id) and details. Use the con_id in later calls to refer to one instrument unambiguously. For options, call get_option_chain first and then search with expiry, strike and right; a bare option symbol matches thousands of contracts.",
		readOnly, s.searchContracts)
	addTool(s, "get_quote", "Get quote",
		"One-off market data snapshot (bid, ask, last, sizes, open/high/low/close, volume) for one contract. market_data_type tells whether the data is live or delayed. Takes up to ~11s when some fields are unavailable.",
		readOnly, s.getQuote)
	addTool(s, "get_historical_bars", "Get historical bars",
		"Historical OHLCV bars for one contract. duration is '<n> <S|D|W|M|Y>' (e.g. '5 D', '1 Y'); IB limits how many bars one request may return for small bar sizes. Each bar's t: for intraday bars its start time (RFC 3339); for daily and longer bars the date IB labels it with (YYYY-MM-DD), the session's trade date, so a futures session that opens the evening before carries the day it closes.",
		readOnly, s.getHistoricalBars)
	addTool(s, "get_option_chain", "Get option chain",
		"Option expirations and strikes for an underlying, per exchange and trading class. Use the result to build option contracts (sec_type OPT, expiry, strike, right).",
		readOnly, s.getOptionChain)
	addTool(s, "get_positions", "Get positions",
		"Current positions of all managed accounts (or one account) with average cost.",
		readOnly, s.getPositions)
	addTool(s, "get_account_summary", "Get account summary",
		"Account values such as NetLiquidation, TotalCashValue, BuyingPower, AvailableFunds, margin requirements.",
		readOnly, s.getAccountSummary)
	addTool(s, "list_orders", "List orders",
		"Open orders of every API client and manual TWS orders (other clients' orders are asked from TWS on each call), or with scope=all also the filled and cancelled orders this session has seen. own=true marks orders of this API client, which are the only ones the trading tools can modify or cancel.",
		readOnly, s.listOrders)
	addTool(s, "get_order", "Get order",
		"Full state of one order: status, fills with commissions, and its event log (including rejection reasons). Orders of other clients are checked with TWS; stale=true means the status is the last one seen and may be out of date (note says why).",
		readOnly, s.getOrder)
	addTool(s, "get_executions", "Get executions",
		"Today's executions (fills) with commission and realized P&L, optionally filtered.",
		readOnly, s.getExecutions)
	addTool(s, "preview_order", "Preview order",
		"Validate an order without sending it: resolves the contract, checks it against the server policy, quotes the market, and asks IB for the margin and commission impact with a what-if order, which IB evaluates but never executes (get_status shows whether what-if is enabled). Supports new orders, brackets (take_profit_price + stop_loss_price on a LMT entry) and modifications of an open order (modify_order_id). When trading is enabled it returns a single-use preview_id that place_order needs. Show the preview to the user and get explicit confirmation before placing.",
		toolAnnotations{ReadOnlyHint: true, OpenWorldHint: true}, s.previewOrder)

	if !s.cfg.Trading.Enabled {
		return
	}
	addTool(s, "place_order", "Place previewed order",
		"Send exactly the order of a preview_order result to the broker. Only call this after the user explicitly confirmed that preview. The preview id is single-use and expires. The risk manager may still reject the order; do not try to get around a rejection (for example by splitting the order).",
		destructive, s.placeOrder)
	addTool(s, "cancel_order", "Cancel order",
		"Cancel one open order of this API client by order_id.",
		destructive, s.cancelOrder)
	addTool(s, "cancel_all_orders", "Cancel all orders",
		"Cancel every open order of this API client. Orders placed by other clients or manually in TWS are not touched.",
		destructive, s.cancelAllOrders)
	addTool(s, "halt_trading", "Halt trading",
		"Kill switch: reject all new orders and modifications until a human restarts the server. Open orders stay working unless cancel_open_orders is true. Use it whenever something looks wrong. Agents cannot resume trading.",
		toolAnnotations{DestructiveHint: true, IdempotentHint: true, OpenWorldHint: true}, s.haltTrading)
}

func (s *Server) instructions() string {
	var b strings.Builder
	b.WriteString("Interactive Brokers via TWS/IB Gateway. ")
	b.WriteString("Start with get_status. Identify instruments with search_contracts and refer to them by con_id afterwards. ")
	b.WriteString("Market data, account data and order state are read-only and safe to call. ")
	if !s.cfg.Trading.Enabled {
		if s.cfg.DisableWhatIf {
			b.WriteString("Trading is disabled on this server: preview_order validates and quotes orders but nothing can be placed.")
		} else {
			b.WriteString("Trading is disabled on this server: preview_order works as a what-if calculator but nothing can be placed.")
		}
		return b.String()
	}
	b.WriteString("Trading rules: (1) every order is first sent to preview_order; (2) show the full preview (contract, side, quantity, price, estimated notional, margin impact, warnings) to the user; ")
	b.WriteString("(3) call place_order only after the user explicitly confirms that exact preview, never on your own initiative; ")
	b.WriteString("(4) a risk rejection is final: report it, do not split, resize or re-route the order to get around it; ")
	b.WriteString("(5) call halt_trading if anything looks wrong (unexpected fills, positions or errors). ")
	if s.cfg.Trading.AllowLive {
		b.WriteString("WARNING: live accounts are enabled, orders can trade real money.")
	} else {
		b.WriteString("Only paper accounts can trade.")
	}
	return b.String()
}

// --- shared helpers ---

func (s *Server) requireConnected() error {
	if !s.b.IsConnected() {
		if p := s.connectionProblem(); p != "" {
			return fmt.Errorf("not connected to TWS/IB Gateway: %s (the server keeps retrying; check get_status)", p)
		}
		return errors.New("not connected to TWS/IB Gateway (the server keeps retrying; check get_status)")
	}
	return nil
}

// connectionProblem says why the last connection attempt failed, or "" when
// none failed since the last successful connection.
func (s *Server) connectionProblem() string {
	err := s.b.LastConnectError()
	if err == nil {
		return ""
	}
	msg := err.Error()
	if errors.Is(err, ibgo.ErrClientIDInUse) {
		msg += " — another API client is connected with this client id, for example an ib-mcp started by another agent session: stop it, or start this server with a different -client-id (env IB_CLIENT_ID)"
	}
	return msg
}

// ownClientID returns the API client id whose orders the trading tools may
// place, modify and cancel. Client id 0 is refused: TWS binds manual TWS
// orders to it, so its orders would include orders placed by hand.
func (s *Server) ownClientID() (int64, error) {
	id := s.b.ClientID()
	if id == 0 {
		return 0, errors.New("the trading tools are disabled for API client id 0, to which TWS binds manual TWS orders; run the server with another client id")
	}
	return id, nil
}

// isPaperAccount reports whether an IB account id is a paper trading
// account. Unknown formats count as live.
func isPaperAccount(id string) bool {
	return strings.HasPrefix(id, "DU") || strings.HasPrefix(id, "DF")
}

// price converts an IB double to a JSON number: nil for NaN, infinities,
// UNSET_DOUBLE and IB's -1 "no price".
func price(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v == ibgo.UnsetDouble || v == -1 {
		return nil
	}
	return &v
}

// num converts an IB double to a JSON number: nil for NaN, infinities and
// UNSET_DOUBLE.
func num(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v == ibgo.UnsetDouble {
		return nil
	}
	return &v
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func timeString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
