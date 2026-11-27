// Package fakegw is an in-process fake of TWS/IB Gateway for end-to-end
// tests. It speaks the real wire protocol at server version 178 and simulates
// a small broker: order acknowledgement, modification, cancellation,
// rejection, partial fills driven by the test, bracket (parent/child)
// activation, OCA-style sibling cancellation, what-if responses, open and
// completed order queries, executions with commission reports, positions,
// and dropped connections. Broker state survives reconnects.
//
// The message layouts follow the ib_async decoder/encoder field by field.
package fakegw

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ServerVersion is the protocol version the fake negotiates.
const ServerVersion = 178

// Contract is the subset of contract fields the fake tracks.
type Contract struct {
	ConID        int64
	Symbol       string
	SecType      string
	Exchange     string
	Currency     string
	LocalSymbol  string
	TradingClass string
	Multiplier   string
}

// Order is the broker-side view of an order.
type Order struct {
	OrderID   int64
	ClientID  int64
	PermID    int64
	Contract  Contract
	Action    string
	Qty       float64
	OrderType string
	LmtPrice  float64
	AuxPrice  float64
	TIF       string
	Account   string
	OrderRef  string
	OcaGroup  string
	ParentID  int64
	Transmit  bool

	Status   string
	Filled   float64
	AvgPrice float64
	// held: untransmitted, waiting for the transmitting leg of its group
	held bool
}

// Remaining returns the unfilled quantity.
func (o *Order) Remaining() float64 { return o.Qty - o.Filled }

func (o *Order) done() bool {
	return o.Status == "Filled" || o.Status == "Cancelled" || o.Status == "Inactive"
}

type execution struct {
	ExecID     string
	OrderID    int64
	ClientID   int64
	PermID     int64
	Contract   Contract
	Side       string
	Shares     float64
	Price      float64
	CumQty     float64
	AvgPrice   float64
	Account    string
	Time       time.Time
	Commission float64
	Realized   float64
}

type position struct {
	Account  string
	Contract Contract
	Qty      float64
	AvgCost  float64
}

// Server is a fake TWS/Gateway.
type Server struct {
	ln      net.Listener
	Account string
	// Commission per execution reported in commission reports.
	Commission float64

	mu        sync.Mutex
	conn      net.Conn
	clientID  int64
	writeMu   sync.Mutex
	nextPerm  int64
	nextExec  int
	nextValid int64
	orders    map[int64]*Order // by permId
	execs     []*execution
	positions map[string]*position // account|conId
	received  map[int]int
	lastReq   map[int][]string
	handlers  map[int]func(req []string) [][]byte
	version   int
	reject    func(o *Order) string
	connCh    chan struct{}
	closed    bool
	wg        sync.WaitGroup
}

// Start launches the fake on 127.0.0.1 with a random port.
func Start() (*Server, error) {
	return StartVersion(ServerVersion)
}

// StartVersion is like Start but negotiates the given server version. The
// built-in order simulation assumes 178; other versions are meant for tests
// that only capture requests (LastRequest) or script responses (Handle).
func StartVersion(version int) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:         ln,
		Account:    "DU123456",
		Commission: 1.0,
		nextPerm:   900000,
		nextValid:  1000,
		orders:     map[int64]*Order{},
		positions:  map[string]*position{},
		received:   map[int]int{},
		lastReq:    map[int][]string{},
		handlers:   map[int]func([]string) [][]byte{},
		version:    version,
		connCh:     make(chan struct{}, 16),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Port returns the listening port.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Close stops the server.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	c := s.conn
	s.mu.Unlock()
	s.ln.Close()
	if c != nil {
		c.Close()
	}
	s.wg.Wait()
}

// Connected returns a channel that receives a value each time a client
// completes the handshake.
func (s *Server) Connected() <-chan struct{} { return s.connCh }

// DropConnection closes the current client connection (broker state stays).
func (s *Server) DropConnection() {
	s.mu.Lock()
	c := s.conn
	s.conn = nil
	s.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// SetReject installs a rule: a non-empty return rejects the order with
// error 201 and that reason.
func (s *Server) SetReject(f func(o *Order) string) {
	s.mu.Lock()
	s.reject = f
	s.mu.Unlock()
}

// Handle installs a scripted responder for request message msgID, replacing
// the built-in behaviour. respond gets the request fields (msgId first) and
// returns the messages to send back, built with Msg.
func (s *Server) Handle(msgID int, respond func(req []string) [][]byte) {
	s.mu.Lock()
	s.handlers[msgID] = respond
	s.mu.Unlock()
}

// LastRequest returns the fields of the last received message with msgID.
func (s *Server) LastRequest(msgID int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lastReq[msgID]...)
}

// Msg encodes values as one TWS message (ints, strings, floats, bools;
// math.MaxFloat64 is sent as TWS's UNSET_DOUBLE).
func Msg(vals ...interface{}) []byte { return msg(vals...) }

// Received returns how many messages with msgID the fake has received.
func (s *Server) Received(msgID int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received[msgID]
}

// Order returns a copy of the broker order with the given client order id.
func (s *Server) Order(clientID, orderID int64) (Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.findLocked(clientID, orderID); o != nil {
		return *o, true
	}
	return Order{}, false
}

// AddOrder adds a working order placed elsewhere: by another API client, or
// manually in TWS (ClientID and OrderID 0). Like TWS, the fake reports it
// only to reqAllOpenOrders (and to reqOpenOrders of its own client) and
// sends no events for it. It returns the order's perm id.
func (s *Server) AddOrder(o Order) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextPerm++
	o.PermID = s.nextPerm
	o.Transmit = true
	if o.Status == "" {
		o.Status = "Submitted"
	}
	s.orders[o.PermID] = &o
	return o.PermID
}

// SetOrderStatus changes the status of an order added with AddOrder, without
// telling the client (it is another client's order).
func (s *Server) SetOrderStatus(permID int64, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.orders[permID]; o != nil {
		o.Status = status
	}
}

// SetPosition seeds a position.
func (s *Server) SetPosition(c Contract, qty, avgCost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.positions[s.Account+"|"+strconv.FormatInt(c.ConID, 10)] = &position{Account: s.Account, Contract: c, Qty: qty, AvgCost: avgCost}
}

// Fill executes qty of the order at price. Messages are sent only if a
// client is connected; the broker state changes either way (fills while
// disconnected are what SyncOrders must recover).
func (s *Server) Fill(clientID, orderID int64, qty, price float64) error {
	s.mu.Lock()
	o := s.findLocked(clientID, orderID)
	if o == nil {
		s.mu.Unlock()
		return fmt.Errorf("fakegw: unknown order %d/%d", clientID, orderID)
	}
	if o.done() || o.held || o.Status == "PreSubmitted" {
		s.mu.Unlock()
		return fmt.Errorf("fakegw: order %d not fillable (status %s, held %v)", orderID, o.Status, o.held)
	}
	if qty > o.Remaining()+1e-9 {
		s.mu.Unlock()
		return fmt.Errorf("fakegw: fill %g exceeds remaining %g", qty, o.Remaining())
	}
	msgs := s.fillLocked(o, qty, price)
	s.mu.Unlock()
	s.sendAll(msgs)
	return nil
}

func (s *Server) findLocked(clientID, orderID int64) *Order {
	for _, o := range s.orders {
		if o.ClientID == clientID && o.OrderID == orderID {
			return o
		}
	}
	return nil
}

// --- connection handling ---

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.handleConn(c)
	}
}

func (s *Server) handleConn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)

	// Handshake: "API\0" + length-prefixed version string.
	prefix := make([]byte, 4)
	if _, err := io.ReadFull(br, prefix); err != nil || string(prefix) != "API\x00" {
		return
	}
	if _, err := readFrame(br); err != nil {
		return
	}
	s.mu.Lock()
	version := s.version
	s.mu.Unlock()
	if err := writeFrame(c, &s.writeMu, fields(strconv.Itoa(version), time.Now().Format("20060102 15:04:05 MST"))); err != nil {
		return
	}
	// startApi: 71, version, clientId, optCapab
	start, err := readFields(br)
	if err != nil || len(start) < 3 || start[0] != "71" {
		return
	}
	clientID, _ := strconv.ParseInt(start[2], 10, 64)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.conn = c
	s.clientID = clientID
	nextValid := s.nextValid
	for _, o := range s.orders {
		if o.ClientID == clientID && o.OrderID >= nextValid {
			nextValid = o.OrderID + 1
		}
	}
	acct := s.Account
	s.mu.Unlock()

	s.send(c, msg(9, 1, nextValid))
	s.send(c, msg(15, 1, acct))
	select {
	case s.connCh <- struct{}{}:
	default:
	}

	for {
		f, err := readFields(br)
		if err != nil {
			break
		}
		s.dispatch(c, f)
	}

	s.mu.Lock()
	if s.conn == c {
		s.conn = nil
	}
	s.mu.Unlock()
}

func (s *Server) dispatch(c net.Conn, f []string) {
	if len(f) == 0 {
		return
	}
	id, _ := strconv.Atoi(f[0])
	s.mu.Lock()
	s.received[id]++
	s.lastReq[id] = append([]string(nil), f...)
	custom := s.handlers[id]
	s.mu.Unlock()
	if custom != nil {
		for _, m := range custom(f) {
			s.send(c, m)
		}
		return
	}

	switch id {
	case 3: // placeOrder
		s.onPlaceOrder(c, f)
	case 4: // cancelOrder: 4, version, orderId, manualCancelOrderTime
		s.onCancel(c, atoi(f, 2))
	case 5: // reqOpenOrders
		s.onOpenOrders(c, false)
	case 16: // reqAllOpenOrders
		s.onOpenOrders(c, true)
	case 7: // reqExecutions: 7, version, reqId, filter...
		s.onExecutions(c, atoi(f, 2))
	case 61: // reqPositions
		s.onPositions(c)
	case 99: // reqCompletedOrders
		s.onCompleted(c)
	case 49: // reqCurrentTime
		s.send(c, msg(49, 1, time.Now().Unix()))
	case 58: // reqGlobalCancel
		s.onGlobalCancel(c)
	}
}

// --- order handling ---

type placed struct {
	Order
	whatIf bool
}

func (s *Server) onPlaceOrder(c net.Conn, f []string) {
	p, err := parsePlaceOrder(f)
	if err != nil {
		s.send(c, errMsg(-1, 320, "fakegw: "+err.Error()))
		return
	}
	if p.whatIf {
		o := p.Order
		o.Status = "PreSubmitted"
		// TWS first sends an openOrder without margin numbers, then the real one.
		s.send(c, encodeOpenOrder(&o, true, nil))
		s.send(c, encodeOpenOrder(&o, true, &whatIfNumbers{init: "1500.25", maint: "1200.5", ewl: "100000.0", commission: 1.25}))
		return
	}

	s.mu.Lock()
	p.ClientID = s.clientID
	if existing := s.findLocked(p.ClientID, p.OrderID); existing != nil {
		// Modification of a live order.
		if existing.done() {
			s.mu.Unlock()
			s.send(c, errMsg(p.OrderID, 104, "Cannot modify a filled order."))
			return
		}
		existing.Qty = p.Qty
		existing.LmtPrice = p.LmtPrice
		existing.AuxPrice = p.AuxPrice
		existing.OrderType = p.OrderType
		existing.OrderRef = p.OrderRef
		existing.TIF = p.TIF
		o := *existing
		s.mu.Unlock()
		s.send(c, encodeOpenOrder(&o, false, nil))
		s.send(c, encodeOrderStatus(&o))
		return
	}
	if s.reject != nil {
		if reason := s.reject(&p.Order); reason != "" {
			s.mu.Unlock()
			s.send(c, errMsg(p.OrderID, 201, "Order rejected - reason:"+reason))
			return
		}
	}
	o := p.Order
	s.nextPerm++
	o.PermID = s.nextPerm
	s.orders[o.PermID] = &o
	var msgs [][]byte
	if !o.Transmit {
		o.held = true
	} else {
		msgs = s.activateLocked(&o)
	}
	s.mu.Unlock()
	s.sendAll(msgs)
}

// activateLocked transmits o (and the held legs of its group).
func (s *Server) activateLocked(o *Order) [][]byte {
	var msgs [][]byte
	group := []*Order{o}
	if o.ParentID != 0 {
		if parent := s.findLocked(o.ClientID, o.ParentID); parent != nil {
			group = []*Order{parent}
			for _, x := range s.orders {
				if x.ClientID == o.ClientID && x.ParentID == o.ParentID {
					group = append(group, x)
				}
			}
		}
	}
	for _, x := range group {
		x.held = false
		if x.ParentID != 0 {
			x.Status = "PreSubmitted" // child waits for the parent fill
		} else {
			x.Status = "Submitted"
		}
		cp := *x
		msgs = append(msgs, encodeOpenOrder(&cp, false, nil), encodeOrderStatus(&cp))
	}
	if o.ParentID == 0 && o.OrderType == "MKT" {
		// Market orders fill immediately at the limit/aux price or 100.
		px := 100.0
		msgs = append(msgs, s.fillLocked(o, o.Remaining(), px)...)
	}
	return msgs
}

func (s *Server) fillLocked(o *Order, qty, price float64) [][]byte {
	s.nextExec++
	notional := o.AvgPrice*o.Filled + qty*price
	o.Filled += qty
	o.AvgPrice = notional / o.Filled
	if o.Remaining() <= 1e-9 {
		o.Status = "Filled"
	} else {
		o.Status = "Submitted"
	}

	side := "BOT"
	signed := qty
	if o.Action == "SELL" {
		side = "SLD"
		signed = -qty
	}
	realized := s.updatePositionLocked(o.Contract, signed, price)
	ex := &execution{
		ExecID:     fmt.Sprintf("0000e1f4.%08d.01.01", s.nextExec),
		OrderID:    o.OrderID,
		ClientID:   o.ClientID,
		PermID:     o.PermID,
		Contract:   o.Contract,
		Side:       side,
		Shares:     qty,
		Price:      price,
		CumQty:     o.Filled,
		AvgPrice:   o.AvgPrice,
		Account:    s.Account,
		Time:       time.Now(),
		Commission: s.Commission,
		Realized:   realized,
	}
	s.execs = append(s.execs, ex)

	cp := *o
	msgs := [][]byte{encodeExecDetails(-1, ex), encodeOrderStatus(&cp), encodeCommission(ex)}

	if o.Status == "Filled" {
		// Parent filled: release children. Child filled: cancel siblings.
		for _, x := range s.orders {
			if x.ClientID != o.ClientID || x.done() {
				continue
			}
			switch {
			case x.ParentID == o.OrderID && o.ParentID == 0:
				x.Status = "Submitted"
				c := *x
				msgs = append(msgs, encodeOrderStatus(&c))
			case o.ParentID != 0 && x.ParentID == o.ParentID && x.PermID != o.PermID:
				x.Status = "Cancelled"
				c := *x
				msgs = append(msgs, encodeOrderStatus(&c))
			case o.OcaGroup != "" && x.OcaGroup == o.OcaGroup && x.PermID != o.PermID:
				x.Status = "Cancelled"
				c := *x
				msgs = append(msgs, encodeOrderStatus(&c))
			}
		}
	}
	return msgs
}

// updatePositionLocked applies a signed fill and returns realized P&L.
func (s *Server) updatePositionLocked(c Contract, signed, price float64) float64 {
	key := s.Account + "|" + strconv.FormatInt(c.ConID, 10)
	p := s.positions[key]
	if p == nil {
		p = &position{Account: s.Account, Contract: c}
		s.positions[key] = p
	}
	realized := 0.0
	if p.Qty != 0 && (p.Qty > 0) != (signed > 0) {
		closing := math.Min(math.Abs(signed), math.Abs(p.Qty))
		if p.Qty > 0 {
			realized = closing * (price - p.AvgCost)
		} else {
			realized = closing * (p.AvgCost - price)
		}
	}
	newQty := p.Qty + signed
	switch {
	case newQty == 0:
		p.AvgCost = 0
	case p.Qty == 0 || (p.Qty > 0) != (newQty > 0):
		p.AvgCost = price
	case (p.Qty > 0) == (signed > 0):
		p.AvgCost = (p.AvgCost*math.Abs(p.Qty) + price*math.Abs(signed)) / math.Abs(newQty)
	}
	p.Qty = newQty
	return realized
}

func (s *Server) onCancel(c net.Conn, orderID int64) {
	s.mu.Lock()
	o := s.findLocked(s.clientID, orderID)
	if o == nil || o.done() {
		state := "Unknown"
		if o != nil {
			state = o.Status
		}
		s.mu.Unlock()
		s.send(c, errMsg(orderID, 10148, fmt.Sprintf("OrderId %d that needs to be cancelled cannot be cancelled, state: %s.", orderID, state)))
		return
	}
	var msgs [][]byte
	for _, x := range s.cancelGroupLocked(o) {
		cp := *x
		msgs = append(msgs, encodeOrderStatus(&cp))
	}
	s.mu.Unlock()
	s.sendAll(msgs)
	s.send(c, errMsg(orderID, 202, "Order Canceled - reason:"))
}

// cancelGroupLocked cancels o and, for a parent, its children.
func (s *Server) cancelGroupLocked(o *Order) []*Order {
	out := []*Order{o}
	o.Status = "Cancelled"
	o.held = false
	if o.ParentID == 0 {
		for _, x := range s.orders {
			if x.ClientID == o.ClientID && x.ParentID == o.OrderID && !x.done() {
				x.Status = "Cancelled"
				x.held = false
				out = append(out, x)
			}
		}
	}
	return out
}

func (s *Server) onGlobalCancel(c net.Conn) {
	s.mu.Lock()
	var msgs [][]byte
	for _, o := range s.orders {
		if !o.done() {
			o.Status = "Cancelled"
			cp := *o
			msgs = append(msgs, encodeOrderStatus(&cp))
		}
	}
	s.mu.Unlock()
	s.sendAll(msgs)
}

func (s *Server) onOpenOrders(c net.Conn, all bool) {
	s.mu.Lock()
	var msgs [][]byte
	for _, o := range s.sortedOrdersLocked() {
		if o.done() || (!all && o.ClientID != s.clientID) {
			continue
		}
		cp := *o
		msgs = append(msgs, encodeOpenOrder(&cp, false, nil), encodeOrderStatus(&cp))
	}
	s.mu.Unlock()
	s.sendAll(msgs)
	s.send(c, msg(53, 1))
}

func (s *Server) onExecutions(c net.Conn, reqID int64) {
	s.mu.Lock()
	var msgs [][]byte
	for _, ex := range s.execs {
		msgs = append(msgs, encodeExecDetails(reqID, ex), encodeCommission(ex))
	}
	s.mu.Unlock()
	s.sendAll(msgs)
	s.send(c, msg(55, 1, reqID))
}

func (s *Server) onPositions(c net.Conn) {
	s.mu.Lock()
	var msgs [][]byte
	for _, p := range s.positions {
		k := p.Contract
		msgs = append(msgs, msg(61, 3, p.Account, k.ConID, k.Symbol, k.SecType, "", 0.0, "", k.Multiplier,
			k.Exchange, k.Currency, k.LocalSymbol, k.TradingClass, p.Qty, p.AvgCost))
	}
	s.mu.Unlock()
	s.sendAll(msgs)
	s.send(c, msg(62, 1))
}

func (s *Server) onCompleted(c net.Conn) {
	s.mu.Lock()
	var msgs [][]byte
	for _, o := range s.sortedOrdersLocked() {
		if o.done() {
			cp := *o
			msgs = append(msgs, encodeCompletedOrder(&cp))
		}
	}
	s.mu.Unlock()
	s.sendAll(msgs)
	s.send(c, msg(102))
}

func (s *Server) sortedOrdersLocked() []*Order {
	out := make([]*Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, o)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].PermID < out[j-1].PermID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// --- sending ---

func (s *Server) send(c net.Conn, payload []byte) {
	if c == nil {
		return
	}
	_ = writeFrame(c, &s.writeMu, payload)
}

func (s *Server) sendAll(msgs [][]byte) {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return
	}
	for _, m := range msgs {
		s.send(c, m)
	}
}

// --- framing ---

func readFrame(r *bufio.Reader) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr)
	if n > 16<<20 {
		return nil, errors.New("frame too large")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func readFields(r *bufio.Reader) ([]string, error) {
	b, err := readFrame(r)
	if err != nil {
		return nil, err
	}
	f := strings.Split(string(b), "\x00")
	if len(f) > 0 && f[len(f)-1] == "" {
		f = f[:len(f)-1]
	}
	return f, nil
}

func writeFrame(c net.Conn, mu *sync.Mutex, payload []byte) error {
	mu.Lock()
	defer mu.Unlock()
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	copy(frame[4:], payload)
	_, err := c.Write(frame)
	return err
}

func fields(f ...string) []byte {
	return []byte(strings.Join(f, "\x00") + "\x00")
}

// msg encodes values the way TWS does.
func msg(vals ...interface{}) []byte {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = enc(v)
	}
	return fields(out...)
}

func enc(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == math.MaxFloat64 {
			return "1.7976931348623157E308"
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprint(x)
	}
}

func atoi(f []string, i int) int64 {
	if i >= len(f) {
		return 0
	}
	v, _ := strconv.ParseInt(f[i], 10, 64)
	return v
}

func errMsg(reqID int64, code int, text string) []byte {
	// 4, version 2, reqId, code, msg, advancedOrderRejectJson (server >= 166)
	return msg(4, 2, reqID, code, text, "")
}
