package protocol

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/throttle"
)

// newCaptureClient returns a Client whose connection is one end of a pipe,
// and a function returning the next message written to it as fields.
func newCaptureClient(t *testing.T, serverVersion int) (*Client, func() []string) {
	t.Helper()
	server, client := net.Pipe()
	c := NewClient()
	c.throttle = throttle.New(0, 0) // no rate limiting in tests
	c.Conn.conn = client
	c.Conn.reader = bufio.NewReaderSize(client, 64*1024)
	c.SetServerVersion(serverVersion)

	frames := make(chan []string, 64)
	go func() {
		defer close(frames)
		r := bufio.NewReader(server)
		for {
			hdr := make([]byte, 4)
			if _, err := io.ReadFull(r, hdr); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(hdr))
			if _, err := io.ReadFull(r, body); err != nil {
				return
			}
			f := strings.Split(string(body), "\x00")
			if len(f) > 0 && f[len(f)-1] == "" {
				f = f[:len(f)-1]
			}
			frames <- f
		}
	}()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	next := func() []string {
		t.Helper()
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("connection closed before a message was written")
			}
			return f
		case <-time.After(2 * time.Second):
			t.Fatal("no message written")
			return nil
		}
	}
	return c, next
}

func testContract() *contract.Contract {
	return &contract.Contract{
		ConID: 265598, Symbol: "AAPL", SecType: "STK", Exchange: "SMART",
		PrimaryExchange: "NASDAQ", Currency: "USD", LocalSymbol: "AAPL", TradingClass: "NMS",
	}
}

// conFields is the 12-field wire encoding of testContract.
var conFields = []string{"265598", "AAPL", "STK", "", "0", "", "", "SMART", "NASDAQ", "USD", "AAPL", "NMS"}

func join(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func s(v ...string) []string { return v }

// TestRequestEncoding checks every outbound request against the field layout
// of ib_async's client.py (which mirrors TWS's EClient).
func TestRequestEncoding(t *testing.T) {
	bag := &contract.Contract{
		Symbol: "SPREAD", SecType: "BAG", Exchange: "SMART", Currency: "USD",
		ComboLegs: []contract.ComboLeg{
			{ConID: 1, Ratio: 1, Action: "BUY", Exchange: "SMART"},
			{ConID: 2, Ratio: 2, Action: "SELL", Exchange: "SMART"},
		},
		DeltaNeutralContract: &contract.DeltaNeutralContract{ConID: 9, Delta: 0.5, Price: 101.25},
	}
	bagFields := s("0", "SPREAD", "BAG", "", "0", "", "", "SMART", "", "USD", "", "")
	sub := ScannerSubscription{
		NumberOfRows: 10, Instrument: "STK", LocationCode: "STK.US.MAJOR", ScanCode: "TOP_PERC_GAIN",
		AbovePrice: 5, AboveVolume: 1000, ExcludeConvertible: true, StockTypeFilter: "ALL",
	}
	tv := func(k, v string) []contract.TagValue { return []contract.TagValue{{Tag: k, Value: v}} }

	cases := []struct {
		name    string
		version int
		call    func(c *Client) error
		want    []string
	}{
		{"startApi", 178, func(c *Client) error { c.ClientID = 3; return c.StartApi() }, s("71", "2", "3", "")},
		{"reqMktData", 178, func(c *Client) error { return c.ReqMktData(5, testContract(), "233,236", false, true, tv("a", "1")) },
			join(s("1", "11", "5"), conFields, s("0", "233,236", "0", "1", "a=1;"))},
		{"reqMktData BAG + delta neutral", 178, func(c *Client) error { return c.ReqMktData(6, bag, "", true, false, nil) },
			join(s("1", "11", "6"), bagFields, s("2", "1", "1", "BUY", "SMART", "2", "2", "SELL", "SMART"), s("1", "9", "0.5", "101.25"), s("", "1", "0", ""))},
		{"cancelMktData", 178, func(c *Client) error { return c.CancelMktData(5) }, s("2", "2", "5")},
		{"reqContractDetails v178", 178, func(c *Client) error {
			con := testContract()
			con.IncludeExpired, con.SecIDType, con.SecID, con.IssuerID = true, "ISIN", "US0378331005", "e123"
			return c.ReqContractDetails(7, con)
		}, join(s("9", "8", "7"), conFields, s("1", "ISIN", "US0378331005", "e123"))},
		{"reqContractDetails v175 (no issuerId)", 175, func(c *Client) error { return c.ReqContractDetails(7, testContract()) },
			join(s("9", "8", "7"), conFields, s("0", "", ""))},
		{"reqHistoricalData", 178, func(c *Client) error {
			return c.ReqHistoricalData(8, testContract(), "20231019 16:00:00", "1 D", "1 hour", "TRADES", true, 1, false, nil)
		}, join(s("20", "8"), conFields, s("0", "20231019 16:00:00", "1 hour", "1 D", "1", "TRADES", "1", "0", ""))},
		{"reqHistoricalData BAG", 178, func(c *Client) error {
			return c.ReqHistoricalData(8, bag, "", "1 D", "1 day", "MIDPOINT", false, 2, true, tv("x", "y"))
		}, join(s("20", "8"), bagFields, s("0", "", "1 day", "1 D", "0", "MIDPOINT", "2"), s("2", "1", "1", "BUY", "SMART", "2", "2", "SELL", "SMART"), s("1", "x=y;"))},
		{"cancelHistoricalData", 178, func(c *Client) error { return c.CancelHistoricalData(8) }, s("25", "1", "8")},
		{"reqOpenOrders", 178, func(c *Client) error { return c.ReqOpenOrders() }, s("5", "1")},
		{"reqAccountUpdates", 178, func(c *Client) error { return c.ReqAccountUpdates(true, "DU1") }, s("6", "2", "1", "DU1")},
		{"reqExecutions", 178, func(c *Client) error {
			return c.ReqExecutions(9, ExecutionFilter{ClientID: 7, AcctCode: "DU1", Time: "20231019 00:00:00", Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Side: "BUY"})
		}, s("7", "3", "9", "7", "DU1", "20231019 00:00:00", "AAPL", "STK", "SMART", "BUY")},
		{"reqIds", 178, func(c *Client) error { return c.ReqIds(1) }, s("8", "1", "1")},
		{"reqPositions", 178, func(c *Client) error { return c.ReqPositions() }, s("61", "1")},
		{"cancelPositions", 178, func(c *Client) error { return c.CancelPositions() }, s("64", "1")},
		{"reqAccountSummary", 178, func(c *Client) error { return c.ReqAccountSummary(10, "All", "NetLiquidation,BuyingPower") },
			s("62", "1", "10", "All", "NetLiquidation,BuyingPower")},
		{"cancelAccountSummary", 178, func(c *Client) error { return c.CancelAccountSummary(10) }, s("63", "1", "10")},
		{"cancelOrder v178", 178, func(c *Client) error { return c.CancelOrder(11, "20231019 10:00:00") }, s("4", "1", "11", "20231019 10:00:00")},
		{"cancelOrder v168 (no manual time)", 168, func(c *Client) error { return c.CancelOrder(11, "") }, s("4", "1", "11")},
		{"reqGlobalCancel", 178, func(c *Client) error { return c.ReqGlobalCancel() }, s("58", "1")},
		{"reqAllOpenOrders", 178, func(c *Client) error { return c.ReqAllOpenOrders() }, s("16", "1")},
		{"reqAutoOpenOrders", 178, func(c *Client) error { return c.ReqAutoOpenOrders(true) }, s("15", "1", "1")},
		{"reqCurrentTime", 178, func(c *Client) error { return c.ReqCurrentTime() }, s("49", "1")},
		{"reqCompletedOrders", 178, func(c *Client) error { return c.ReqCompletedOrders(false) }, s("99", "0")},
		{"reqMatchingSymbols", 178, func(c *Client) error { return c.ReqMatchingSymbols(12, "AA") }, s("81", "12", "AA")},
		{"reqMarketDataType", 178, func(c *Client) error { return c.ReqMarketDataType(3) }, s("59", "1", "3")},
		{"reqMarketRule", 178, func(c *Client) error { return c.ReqMarketRule(26) }, s("91", "26")},
		{"reqPnL", 178, func(c *Client) error { return c.ReqPnL(13, "DU1", "") }, s("92", "13", "DU1", "")},
		{"cancelPnL", 178, func(c *Client) error { return c.CancelPnL(13) }, s("93", "13")},
		{"reqPnLSingle", 178, func(c *Client) error { return c.ReqPnLSingle(14, "DU1", "", 265598) }, s("94", "14", "DU1", "", "265598")},
		{"cancelPnLSingle", 178, func(c *Client) error { return c.CancelPnLSingle(14) }, s("95", "14")},
		{"reqSecDefOptParams", 178, func(c *Client) error { return c.ReqSecDefOptParams(15, "AAPL", "", "STK", 265598) },
			s("78", "15", "AAPL", "", "STK", "265598")},
		{"reqNewsProviders", 178, func(c *Client) error { return c.ReqNewsProviders() }, s("85")},
		{"reqNewsArticle", 178, func(c *Client) error { return c.ReqNewsArticle(16, "BRFG", "BRFG$123", nil) }, s("84", "16", "BRFG", "BRFG$123", "")},
		{"reqHistoricalNews", 178, func(c *Client) error {
			return c.ReqHistoricalNews(17, 265598, "BRFG+DJNL", "2023-10-01 00:00:00", "2023-10-19 00:00:00", 10, nil)
		}, s("86", "17", "265598", "BRFG+DJNL", "2023-10-01 00:00:00", "2023-10-19 00:00:00", "10", "")},
		{"reqHeadTimestamp (useRTH before whatToShow)", 178, func(c *Client) error { return c.ReqHeadTimestamp(18, testContract(), "TRADES", true, 1) },
			join(s("87", "18"), conFields, s("0", "1", "TRADES", "1"))},
		{"cancelHeadTimestamp", 178, func(c *Client) error { return c.CancelHeadTimestamp(18) }, s("90", "18")},
		{"reqRealTimeBars", 178, func(c *Client) error { return c.ReqRealTimeBars(19, testContract(), 5, "MIDPOINT", false, nil) },
			join(s("50", "3", "19"), conFields, s("5", "MIDPOINT", "0", ""))},
		{"cancelRealTimeBars", 178, func(c *Client) error { return c.CancelRealTimeBars(19) }, s("51", "1", "19")},
		{"reqScannerSubscription (filter options first)", 178, func(c *Client) error {
			return c.ReqScannerSubscription(20, sub, tv("opt", "1"), tv("priceAbove", "5"))
		}, s("22", "20", "10", "STK", "STK.US.MAJOR", "TOP_PERC_GAIN", "5", "0", "1000", "0", "0", "", "", "", "", "", "", "0", "0", "1", "0", "", "ALL",
			"priceAbove=5;", "opt=1;")},
		{"cancelScannerSubscription", 178, func(c *Client) error { return c.CancelScannerSubscription(20) }, s("23", "1", "20")},
		{"reqScannerParameters", 178, func(c *Client) error { return c.ReqScannerParameters() }, s("24", "1")},
		{"reqFamilyCodes", 178, func(c *Client) error { return c.ReqFamilyCodes() }, s("80")},
		{"reqSmartComponents", 178, func(c *Client) error { return c.ReqSmartComponents(21, "a6") }, s("83", "21", "a6")},
		{"reqMktDepthExchanges", 178, func(c *Client) error { return c.ReqMktDepthExchanges() }, s("82")},
		{"reqSoftDollarTiers", 178, func(c *Client) error { return c.ReqSoftDollarTiers(22) }, s("79", "22")},
		{"reqHistogramData", 178, func(c *Client) error { return c.ReqHistogramData(23, testContract(), true, "1 week") },
			join(s("88", "23"), conFields, s("0", "1", "1 week"))},
		{"cancelHistogramData", 178, func(c *Client) error { return c.CancelHistogramData(23) }, s("89", "23")},
		{"reqUserInfo", 178, func(c *Client) error { return c.ReqUserInfo(24) }, s("104", "24")},
		{"reqMktDepth", 178, func(c *Client) error { return c.ReqMktDepth(25, testContract(), 5, true, nil) },
			join(s("10", "5", "25"), conFields, s("5", "1", ""))},
		{"cancelMktDepth", 178, func(c *Client) error { return c.CancelMktDepth(25, true) }, s("11", "1", "25", "1")},
		{"reqTickByTickData", 178, func(c *Client) error { return c.ReqTickByTickData(26, testContract(), "BidAsk", 0, false) },
			join(s("97", "26"), conFields, s("BidAsk", "0", "0"))},
		{"cancelTickByTickData", 178, func(c *Client) error { return c.CancelTickByTickData(26) }, s("98", "26")},
		{"reqHistoricalTicks", 178, func(c *Client) error {
			return c.ReqHistoricalTicks(27, testContract(), "", "20261125 15:59:00 US/Eastern", 100, "TRADES", true, false, nil)
		}, join(s("96", "27"), conFields, s("0", "", "20261125 15:59:00 US/Eastern", "100", "TRADES", "1", "0", ""))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, next := newCaptureClient(t, tc.version)
			errCh := make(chan error, 1)
			go func() { errCh <- tc.call(c) }()
			got := next()
			if err := <-errCh; err != nil {
				t.Fatalf("send: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("fields mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestSendWhenNotConnected(t *testing.T) {
	c := NewClient()
	if err := c.ReqPositions(); err == nil {
		t.Fatal("Send must fail without a connection")
	}
}
