// mdcheck reports which market data the connected account receives through
// the API. It requests LIVE data (market data type 1), so a missing
// subscription shows up as an error instead of a silent fallback to delayed
// data. It checks one contract per US venue group:
//
//   - US stocks: NYSE (Network A), NYSE Arca (Network B), NASDAQ (Network C)
//   - US options (OPRA): the nearest SPY and AAPL call at the money
//   - futures: CME, CBOT, NYMEX, COMEX and CFE front months
//   - the SPX index (CBOE)
//
// plus tick-by-tick, 5-second bars and market depth (L2) on SPY, AAPL and
// MES. It never calls an order API; on a live account run it through
// cmd/ibguard:
//
//	go run ./cmd/ibguard -upstream 127.0.0.1:4001 -listen 127.0.0.1:4011 &
//	go run ./cmd/mdcheck -port 4011
//
// Results depend on the trading session: outside US market hours stocks
// and options have no live quotes (bid/ask -1) and stock tick-by-tick and
// 5-second bars stay empty, without any permission error.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
)

var (
	host     = flag.String("host", envOr("IB_HOST", "127.0.0.1"), "TWS/Gateway host")
	port     = flag.Int("port", envIntOr("IB_PORT", 4002), "TWS/Gateway port (or the ibguard port)")
	clientID = flag.Int("client-id", envIntOr("IB_MDCHECK_CLIENT_ID", 98), "API client id")
	watch    = flag.Duration("watch", 12*time.Second, "how long to stream before reporting")
)

// Error codes about market data permissions, with the verdict they mean.
var permissionCodes = map[int]string{
	354:   "NOT SUBSCRIBED",
	10089: "NOT SUBSCRIBED",
	10168: "NOT SUBSCRIBED",
	10090: "PARTIALLY SUBSCRIBED",
	10091: "PARTIALLY SUBSCRIBED",
	10197: "COMPETING LIVE SESSION",
}

// errLog records request errors by the contract pointer of the request
// (IBError.Contract). Every probe sends its own copy of the contract, so an
// error is attributed to the request that caused it whenever it arrives.
type errLog struct {
	mu     sync.Mutex
	byCon  map[*contract.Contract][]*ibgo.IBError
	others []string
}

func (l *errLog) add(e *ibgo.IBError) {
	if e.ReqID < 0 || e.Code >= 2100 && e.Code < 2200 { // connection/farm notices
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.Contract != nil {
		l.byCon[e.Contract] = append(l.byCon[e.Contract], e)
		return
	}
	l.others = append(l.others, fmt.Sprintf("[reqId=%d] %d %.100s", e.ReqID, e.Code, e.Message))
}

// get returns the errors of the requests sent with con.
func (l *errLog) get(con *contract.Contract) []*ibgo.IBError {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*ibgo.IBError(nil), l.byCon[con]...)
}

// probeCopy returns a copy of c for one probe (see errLog).
func probeCopy(c *contract.Contract) *contract.Contract {
	cp := *c
	return &cp
}

// counts are accumulated in UpdateEvent callbacks: the per-update tick
// lists of a Ticker are cleared after each update.
type counts struct{ updates, ticks, tickByTicks, domTicks int }

type counter struct {
	mu sync.Mutex
	m  map[*market.Ticker]*counts
}

func (c *counter) watch(t *market.Ticker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[t] != nil {
		return
	}
	n := &counts{}
	c.m[t] = n
	t.UpdateEvent.Subscribe(func(t *market.Ticker) { // reader goroutine
		c.mu.Lock()
		n.updates++
		n.ticks += len(t.Ticks)
		n.tickByTicks += len(t.TickByTicks)
		n.domTicks += len(t.DomTicks)
		c.mu.Unlock()
	})
}

func (c *counter) get(t *market.Ticker) counts {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := c.m[t]; n != nil {
		return *n
	}
	return counts{}
}

type item struct {
	group, label string
	con          *contract.Contract
	ticker       *market.Ticker
}

func main() {
	flag.Parse()
	ib := ibgo.New()
	errs := &errLog{byCon: map[*contract.Contract][]*ibgo.IBError{}}
	ib.ErrorEvent.Subscribe(errs.add)
	ctx, cancel := tctx(15 * time.Second)
	err := ib.Connect(ctx, *host, *port, *clientID)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer ib.Disconnect()
	if err := ib.ReqMarketDataType(ibgo.MarketDataLive); err != nil {
		fmt.Fprintln(os.Stderr, "market data type:", err)
		os.Exit(1)
	}
	now := time.Now()
	fmt.Printf("%s, New York %s (%s; holidays and half days not considered)\n",
		now.Format("2006-01-02 15:04 MST"), nyTime(now).Format("Mon 15:04"), usSession(now))

	items := contracts(ib, now)
	cnt := &counter{m: map[*market.Ticker]*counts{}}

	for i := range items {
		t, err := ib.ReqMktData(items[i].con, "", false, false)
		if err != nil {
			fmt.Printf("ReqMktData %s: %v\n", items[i].label, err)
			continue
		}
		items[i].ticker = t
		cnt.watch(t)
	}
	// Wait until every line has data or an error: permission errors of
	// some farms (e.g. CFE) arrive several seconds after the request.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		settled := true
		for _, it := range items {
			if it.ticker != nil && cnt.get(it.ticker).updates == 0 && len(errs.get(it.con)) == 0 {
				settled = false
			}
		}
		if settled {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Extra probes, each with its own contract copy.
	type probe struct {
		con    *contract.Contract
		ticker *market.Ticker
		bars   *market.RealTimeBarList
	}
	var extra []*contract.Contract
	for _, it := range items {
		if it.con.SecType == "STK" && (it.con.Symbol == "SPY" || it.con.Symbol == "AAPL") || it.con.Symbol == "MES" {
			extra = append(extra, it.con)
		}
	}
	tbt := map[*contract.Contract]*probe{}
	for _, c := range extra {
		p := &probe{con: probeCopy(c)}
		if t, err := ib.ReqTickByTickData(p.con, ibgo.TickByTickBidAsk, 0, false); err == nil {
			p.ticker = t
			tbt[c] = p
			cnt.watch(t)
		}
	}
	time.Sleep(3 * time.Second)
	bars := map[*contract.Contract]*probe{}
	for _, c := range extra {
		p := &probe{con: probeCopy(c)}
		if b, err := ib.ReqRealTimeBars(p.con, 5, "TRADES", false); err == nil {
			p.bars = b
			bars[c] = p
		}
	}
	time.Sleep(3 * time.Second)
	depth := map[*contract.Contract]*probe{}
	for _, c := range extra {
		p := &probe{con: probeCopy(c)}
		if t, err := ib.ReqMktDepth(p.con, 5, c.SecType == "STK"); err == nil {
			p.ticker = t
			depth[c] = p
			cnt.watch(t)
		}
	}
	time.Sleep(3 * time.Second)
	fmt.Printf("streaming for %s ...\n", *watch)
	time.Sleep(*watch)

	group := ""
	for _, it := range items {
		if it.group != group {
			group = it.group
			fmt.Printf("\n== %s ==\n", group)
		}
		if it.ticker == nil {
			continue
		}
		d := it.ticker.Snapshot()
		c := cnt.get(it.ticker)
		e := errs.get(it.con)
		age := "-"
		if !d.LastTimestamp.IsZero() {
			age = time.Since(d.LastTimestamp).Round(time.Second).String()
		}
		greeks := ""
		if g := d.ModelGreeks; g != nil && g.ImpliedVol != nil && g.Delta != nil {
			greeks = fmt.Sprintf(" iv=%.3f delta=%.3f", *g.ImpliedVol, *g.Delta)
		}
		fmt.Printf("  %-30s %-26s upd=%-4d bid=%s ask=%s last=%s close=%s lastAge=%s%s\n",
			it.label, verdict(d, c, e), c.updates, num(d.Bid), num(d.Ask), num(d.Last), num(d.Close), age, greeks)
		printErrs(e)
	}

	fmt.Printf("\n== tick-by-tick (BidAsk) / 5-second bars / depth ==\n")
	for _, c := range extra {
		if p := tbt[c]; p != nil {
			n := cnt.get(p.ticker).tickByTicks
			fmt.Printf("  %-30s %s\n", "tick-by-tick "+c.Symbol, extraVerdict(n, "ticks", errs.get(p.con)))
		}
	}
	for _, c := range extra {
		if p := bars[c]; p != nil {
			n := len(p.bars.Snapshot())
			fmt.Printf("  %-30s %s\n", "5-second bars "+c.Symbol, extraVerdict(n, "bars", errs.get(p.con)))
		}
	}
	for _, c := range extra {
		if p := depth[c]; p != nil {
			d := p.ticker.Snapshot()
			kind := "depth "
			if c.SecType == "STK" {
				kind = "SMART depth "
			}
			fmt.Printf("  %-30s %s (%d bid / %d ask levels)\n", kind+c.Symbol,
				extraVerdict(cnt.get(p.ticker).domTicks, "updates", errs.get(p.con)), len(d.DomBids), len(d.DomAsks))
		}
	}
	errs.mu.Lock()
	if len(errs.others) > 0 {
		fmt.Printf("\n== other errors ==\n  %s\n", strings.Join(errs.others, "\n  "))
	}
	errs.mu.Unlock()

	for _, it := range items {
		if it.ticker != nil {
			ib.CancelMktData(it.con)
		}
	}
	for _, p := range tbt {
		ib.CancelTickByTickData(p.con, ibgo.TickByTickBidAsk)
	}
	for _, p := range bars {
		ib.CancelRealTimeBars(p.bars)
	}
	for _, p := range depth {
		ib.CancelMktDepth(p.con, p.con.SecType == "STK")
	}
	time.Sleep(time.Second)
}

// contracts qualifies the contracts to check; lookups that fail are
// reported and skipped.
func contracts(ib *ibgo.IB, now time.Time) []item {
	var items []item
	for _, s := range []struct{ sym, venue string }{
		{"IBM", "NYSE (Network A)"}, {"SPY", "NYSE Arca (Network B)"}, {"AAPL", "NASDAQ (Network C)"},
	} {
		c := contract.Stock(s.sym, "SMART", "USD")
		if err := qualify(ib, c); err != nil {
			fmt.Printf("qualify %s: %v\n", s.sym, err)
			continue
		}
		items = append(items, item{group: "US stocks", label: s.sym + " " + s.venue, con: c})
	}
	for _, und := range []string{"SPY", "AAPL"} {
		c, err := atmCall(ib, und, now)
		if err != nil {
			fmt.Printf("%s option: %v\n", und, err)
			continue
		}
		items = append(items, item{group: "US options (OPRA)", label: fmt.Sprintf("%s %s %g C", und, c.LastTradeDateOrContractMonth, c.Strike), con: c})
	}
	for _, f := range []struct{ sym, exch, class string }{
		{"MES", "CME", "MES"}, {"ES", "CME", "ES"}, {"YM", "CBOT", "YM"}, {"ZN", "CBOT", "ZN"},
		{"CL", "NYMEX", "CL"}, {"GC", "COMEX", "GC"}, {"VIX", "CFE", "VX"},
	} {
		c, err := frontMonth(ib, f.sym, f.exch, f.class, now)
		if err != nil {
			fmt.Printf("%s future: %v\n", f.sym, err)
			continue
		}
		items = append(items, item{group: "Futures", label: fmt.Sprintf("%s %s (%s)", c.LocalSymbol, c.LastTradeDateOrContractMonth, f.exch), con: c})
	}
	spx := contract.Index("SPX", "CBOE", "USD")
	if err := qualify(ib, spx); err != nil {
		fmt.Printf("SPX: %v\n", err)
	} else {
		items = append(items, item{group: "Index", label: "SPX (CBOE)", con: spx})
	}
	return items
}

func qualify(ib *ibgo.IB, c *contract.Contract) error {
	ctx, cancel := tctx(15 * time.Second)
	defer cancel()
	return ib.QualifyContracts(ctx, c)
}

// atmCall returns the nearest-expiry call of und with the strike closest
// to the last daily close.
func atmCall(ib *ibgo.IB, und string, now time.Time) (*contract.Contract, error) {
	uc := contract.Stock(und, "SMART", "USD")
	if err := qualify(ib, uc); err != nil {
		return nil, err
	}
	ctx, cancel := tctx(20 * time.Second)
	defer cancel()
	bars, err := ib.ReqHistoricalData(ctx, uc, "", "5 D", "1 day", "TRADES", true, 1)
	if err != nil || len(bars) == 0 {
		return nil, fmt.Errorf("last close: %v", err)
	}
	px := bars[len(bars)-1].Close
	chains, err := ib.ReqSecDefOptParams(ctx, und, "", "STK", uc.ConID)
	if err != nil {
		return nil, err
	}
	today := now.Format("20060102")
	for _, ch := range chains {
		if ch.Exchange != "SMART" || ch.TradingClass != und {
			continue
		}
		exps := append([]string(nil), ch.Expirations...)
		sort.Strings(exps)
		var exp string
		for _, e := range exps {
			if e > today {
				exp = e
				break
			}
		}
		strike, best := 0.0, math.Inf(1)
		for _, k := range ch.Strikes {
			if d := math.Abs(k - px); d < best {
				strike, best = k, d
			}
		}
		if exp == "" || strike == 0 {
			break
		}
		c := contract.Option(und, exp, strike, "C", "SMART")
		c.Currency = "USD"
		c.TradingClass = und
		if err := qualify(ib, c); err != nil {
			return nil, err
		}
		return c, nil
	}
	return nil, fmt.Errorf("no SMART option chain")
}

// frontMonth returns the future of the trading class that expires next.
func frontMonth(ib *ibgo.IB, sym, exch, class string, now time.Time) (*contract.Contract, error) {
	ctx, cancel := tctx(15 * time.Second)
	defer cancel()
	c := contract.Future(sym, "", exch)
	c.Currency = "USD"
	c.TradingClass = class
	cds, err := ib.ReqContractDetails(ctx, c)
	if err != nil {
		return nil, err
	}
	today := now.Format("20060102")
	var best *contract.Contract
	for _, cd := range cds {
		e := cd.Contract.LastTradeDateOrContractMonth
		if e > today && (best == nil || e < best.LastTradeDateOrContractMonth) {
			best = cd.Contract
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no unexpired contract")
	}
	return best, nil
}

// verdict summarizes a market data line.
func verdict(d market.TickerData, c counts, errs []*ibgo.IBError) string {
	for _, e := range errs {
		if v := permissionCodes[e.Code]; v != "" {
			return v
		}
	}
	if len(errs) > 0 {
		return fmt.Sprintf("ERROR %d", errs[0].Code)
	}
	switch {
	case d.MarketDataType == ibgo.MarketDataDelayed || d.MarketDataType == ibgo.MarketDataDelayedFrozen:
		return "DELAYED"
	case c.updates == 0:
		return "NO DATA"
	case d.Bid > 0 && d.Ask > 0:
		return "OK live quotes"
	default:
		return "OK no quotes (closed?)"
	}
}

func extraVerdict(n int, unit string, errs []*ibgo.IBError) string {
	for _, e := range errs {
		if v := permissionCodes[e.Code]; v != "" {
			return fmt.Sprintf("%s (%d)", v, e.Code)
		}
	}
	if len(errs) > 0 {
		return fmt.Sprintf("ERROR %d %.80s", errs[0].Code, errs[0].Message)
	}
	if n == 0 {
		return "OK no data (closed?)"
	}
	return fmt.Sprintf("OK %d %s", n, unit)
}

func printErrs(errs []*ibgo.IBError) {
	for _, e := range errs {
		fmt.Printf("  %-30s   error %d: %.100s\n", "", e.Code, e.Message)
	}
}

func num(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func nyTime(t time.Time) time.Time {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return t.In(loc)
	}
	return t.UTC()
}

// usSession names the US equity session at t (regular weekday schedule).
func usSession(t time.Time) string {
	ny := nyTime(t)
	if wd := ny.Weekday(); wd == time.Saturday || wd == time.Sunday && ny.Hour() < 20 || wd == time.Friday && ny.Hour() >= 20 {
		return "weekend"
	}
	m := ny.Hour()*60 + ny.Minute()
	switch {
	case m >= 4*60 && m < 9*60+30:
		return "pre-market"
	case m >= 9*60+30 && m < 16*60:
		return "regular session"
	case m >= 16*60 && m < 20*60:
		return "after hours"
	default:
		return "overnight session"
	}
}

func tctx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
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
