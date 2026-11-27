package fakegw

import (
	"bufio"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawClient is a minimal TWS API client speaking the wire protocol directly.
type rawClient struct {
	t  *testing.T
	c  net.Conn
	r  *bufio.Reader
	mu sync.Mutex
}

func dial(t *testing.T, s *Server, clientID int) (*rawClient, []string) {
	t.Helper()
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	rc := &rawClient{t: t, c: c, r: bufio.NewReader(c)}
	c.Write([]byte("API\x00"))
	var wmu sync.Mutex
	writeFrame(c, &wmu, []byte("v157..178"))
	hello := rc.next()
	rc.send("71", "2", strconv.Itoa(clientID), "")
	rc.expect("9")
	rc.expect("15")
	select {
	case <-s.Connected():
	case <-time.After(2 * time.Second):
		t.Fatal("Connected not signalled")
	}
	return rc, hello
}

func (rc *rawClient) send(f ...string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	writeFrame(rc.c, &sync.Mutex{}, []byte(strings.Join(f, "\x00")+"\x00"))
}

func (rc *rawClient) next() []string {
	rc.t.Helper()
	rc.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	f, err := readFields(rc.r)
	if err != nil {
		rc.t.Fatalf("read: %v", err)
	}
	return f
}

func (rc *rawClient) expect(msgID string) []string {
	rc.t.Helper()
	for i := 0; i < 20; i++ {
		if f := rc.next(); f[0] == msgID {
			return f
		}
	}
	rc.t.Fatalf("message %s not received", msgID)
	return nil
}

// placeOrder fields for a v178 limit order (layout of ib_async client.placeOrder).
func placeOrderFields(orderID int, action string, qty, lmt string, transmit string, parentID int, whatIf bool) []string {
	wi := "0"
	if whatIf {
		wi = "1"
	}
	f := []string{"3", strconv.Itoa(orderID),
		"265598", "AAPL", "STK", "", "0", "", "", "SMART", "", "USD", "AAPL", "NMS", "", "",
		action, qty, "LMT", lmt, "", "", "", "", "O", "0", "", transmit, strconv.Itoa(parentID), "0", "0", "0", "0", "0", "0",
		"", "0", "", "", "", "", ""}
	f = append(f, make([]string, 22)...) // modelCode .. volatilityType
	f = append(f, "", "")                // deltaNeutralOrderType, deltaNeutralAuxPrice
	f = append(f, "0", "", "", "", "", "", "")
	f = append(f, "", "", "", "")
	f = append(f, "0", "", "", "0", "0", "")
	f = append(f, "", wi, "", "0", "0", "0", "0")
	return f
}

func TestHandshakeVersion(t *testing.T) {
	s, err := StartVersion(176)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, hello := dial(t, s, 1)
	if hello[0] != "176" {
		t.Fatalf("server version = %q", hello[0])
	}
}

func TestOrderLifecycleAndErrors(t *testing.T) {
	s := startTestServer(t)
	rc, _ := dial(t, s, 5)

	rc.send(placeOrderFields(1000, "BUY", "10", "100", "1", 0, false)...)
	rc.expect("5") // openOrder
	if st := rc.expect("3"); st[2] != "Submitted" {
		t.Fatalf("status = %q", st)
	}
	o, ok := s.Order(5, 1000)
	if !ok || o.Qty != 10 || o.LmtPrice != 100 || o.Status != "Submitted" || o.Contract.Symbol != "AAPL" {
		t.Fatalf("broker order = %+v", o)
	}

	if err := s.Fill(5, 999, 1, 1); err == nil {
		t.Fatal("unknown order must not fill")
	}
	if err := s.Fill(5, 1000, 11, 1); err == nil {
		t.Fatal("overfill must be rejected")
	}
	if err := s.Fill(5, 1000, 4, 100); err != nil {
		t.Fatal(err)
	}
	ex := rc.expect("11")
	if ex[1] != "-1" || ex[2] != "1000" || ex[18] != "BOT" || ex[19] != "4" {
		t.Fatalf("execDetails = %q", ex)
	}
	if st := rc.expect("3"); st[2] != "Submitted" || st[3] != "4" || st[4] != "6" {
		t.Fatalf("partial status = %q", st)
	}
	if cr := rc.expect("59"); cr[6] != "1.7976931348623157E308" {
		t.Fatalf("opening fill realized P&L must be UNSET: %q", cr)
	}

	rc.send("4", "1", "1000", "")
	if st := rc.expect("3"); st[2] != "Cancelled" {
		t.Fatalf("cancel status = %q", st)
	}
	if e := rc.expect("4"); e[3] != "202" {
		t.Fatalf("cancel error = %q", e)
	}
	if err := s.Fill(5, 1000, 1, 1); err == nil {
		t.Fatal("cancelled order must not fill")
	}
	rc.send("4", "1", "1000", "")
	if e := rc.expect("4"); e[3] != "10148" {
		t.Fatalf("second cancel = %q", e)
	}

	s.SetReject(func(o *Order) string {
		if o.Qty > 100 {
			return "too big"
		}
		return ""
	})
	rc.send(placeOrderFields(1001, "BUY", "500", "1", "1", 0, false)...)
	if e := rc.expect("4"); e[2] != "1001" || e[3] != "201" || !strings.Contains(e[4], "too big") {
		t.Fatalf("reject = %q", e)
	}

	rc.send("3", "1002") // truncated placeOrder
	if e := rc.expect("4"); e[3] != "320" {
		t.Fatalf("malformed placeOrder = %q", e)
	}
	if s.Received(3) != 3 || s.Received(4) != 2 || len(s.LastRequest(4)) != 4 {
		t.Fatalf("received counters: place=%d cancel=%d", s.Received(3), s.Received(4))
	}
}

func startTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestWhatIfIsNotStored(t *testing.T) {
	s := startTestServer(t)
	rc, _ := dial(t, s, 1)
	rc.send(placeOrderFields(2000, "BUY", "1", "1", "1", 0, true)...)
	first := rc.expect("5")
	second := rc.expect("5")
	if slices.Contains(first, "1500.25") || !slices.Contains(second, "1500.25") {
		t.Fatal("the second what-if reply must supply the initial margin change")
	}
	if _, ok := s.Order(1, 2000); ok {
		t.Fatal("what-if orders must not be stored")
	}
}

func TestHeldBracketReleasedByLastLeg(t *testing.T) {
	s := startTestServer(t)
	rc, _ := dial(t, s, 1)
	rc.send(placeOrderFields(10, "BUY", "1", "100", "0", 0, false)...)
	rc.send(placeOrderFields(11, "SELL", "1", "110", "0", 10, false)...)
	rc.send("49", "1")
	rc.expect("49") // both staged orders have been processed
	if o, _ := s.Order(1, 10); !o.held || s.Fill(1, 10, 1, 100) == nil {
		t.Fatal("untransmitted parent must be held and not fillable")
	}
	rc.send(placeOrderFields(12, "SELL", "1", "90", "1", 10, false)...)
	rc.expect("3")
	p, _ := s.Order(1, 10)
	c, _ := s.Order(1, 12)
	if p.Status != "Submitted" || c.Status != "PreSubmitted" {
		t.Fatalf("parent %s child %s", p.Status, c.Status)
	}
	// Cancelling the parent cancels the children.
	rc.send("4", "1", "10", "")
	rc.expect("4") // the gateway reports error 202 after cancelling the group
	for _, id := range []int64{10, 11, 12} {
		if o, _ := s.Order(1, id); o.Status != "Cancelled" {
			t.Fatalf("order %d status %s", id, o.Status)
		}
	}
}

func TestPositionAccounting(t *testing.T) {
	s := startTestServer(t)
	c := Contract{ConID: 1}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.updatePositionLocked(c, 10, 100); r != 0 {
		t.Fatal("opening trade realizes nothing")
	}
	s.updatePositionLocked(c, 10, 110) // avg 105
	if p := s.positions[s.Account+"|1"]; p.Qty != 20 || p.AvgCost != 105 {
		t.Fatalf("position = %+v", p)
	}
	if r := s.updatePositionLocked(c, -5, 115); r != 50 {
		t.Fatalf("partial close realized %g, want 50", r)
	}
	if r := s.updatePositionLocked(c, -25, 100); r != -75 { // close 15 @100 (-75), open short 10
		t.Fatalf("flip realized %g, want -75", r)
	}
	if p := s.positions[s.Account+"|1"]; p.Qty != -10 || p.AvgCost != 100 {
		t.Fatalf("after flip = %+v", p)
	}
	if r := s.updatePositionLocked(c, 10, 90); r != 100 {
		t.Fatalf("closing short realized %g, want 100", r)
	}
	if p := s.positions[s.Account+"|1"]; p.Qty != 0 || p.AvgCost != 0 {
		t.Fatalf("flat = %+v", p)
	}
}

func TestEncodingHelpers(t *testing.T) {
	m := string(Msg(4, 2, int64(-1), 3.5, true, false, math.MaxFloat64, "x", uint8(7)))
	if m != "4\x002\x00-1\x003.5\x001\x000\x001.7976931348623157E308\x00x\x007\x00" {
		t.Fatalf("Msg = %q", m)
	}
	if atoi([]string{"1"}, 5) != 0 {
		t.Fatal("atoi out of range")
	}
	c := &cursor{f: []string{"x", "y", "Infinite", ""}}
	c.int()
	if c.err == nil {
		t.Fatal("bad int must set the cursor error")
	}
	c = &cursor{f: []string{"y"}}
	c.float()
	if c.err == nil {
		t.Fatal("bad float must set the cursor error")
	}
	c = &cursor{f: []string{"Infinite", ""}}
	if !math.IsInf(c.float(), 1) || c.float() != math.MaxFloat64 {
		t.Fatal("Infinite / empty float")
	}
	if _, err := parsePlaceOrder([]string{"3", "1", "0", "", "BAG"}); err == nil {
		t.Fatal("truncated message must fail")
	}
}
