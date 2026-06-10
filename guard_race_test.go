package ibgo

// Concurrency guardrail tests.
//
// The pre-existing handler tests invoke decoder handlers directly on the test
// goroutine, so the race detector never sees the real concurrency: a reader
// goroutine mutating state while user goroutines issue requests and read
// results. These tests drive the production path — a real TCP connection,
// the readLoop goroutine, and concurrent user goroutines — and MUST be run
// with -race (make test-race) to provide their guarantee.

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
)

// guardServer is a scripted in-process TWS: it performs the handshake and
// startApi exchange, then hands every subsequent inbound API message to the
// script callback, which may reply (synchronously or from its own goroutine)
// via send.
type guardServer struct {
	t        *testing.T
	listener net.Listener
	script   func(s *guardServer, msg []string)

	connMu sync.Mutex
	conn   net.Conn

	done chan struct{}
}

func newGuardServer(t *testing.T, script func(s *guardServer, msg []string)) *guardServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &guardServer{t: t, listener: l, script: script, done: make(chan struct{})}
	go s.serve()
	return s
}

func (s *guardServer) port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *guardServer) close() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.listener.Close()
	s.connMu.Lock()
	if s.conn != nil {
		s.conn.Close()
	}
	s.connMu.Unlock()
}

// send frames the fields and writes them in a single Write. It is safe to
// call from any goroutine. Returns false once the connection is gone, so
// streaming scripts can stop.
func (s *guardServer) send(fields ...string) bool {
	payload := []byte(strings.Join(fields, "\x00") + "\x00")
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	copy(frame[4:], payload)

	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn == nil {
		return false
	}
	_, err := s.conn.Write(frame)
	return err == nil
}

func (s *guardServer) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	s.connMu.Lock()
	s.conn = conn
	s.connMu.Unlock()

	br := bufio.NewReader(conn)
	hdr := make([]byte, 4)

	// Handshake: "API\0" + length-prefixed version string
	if _, err := io.ReadFull(br, hdr); err != nil { // "API\0"
		return
	}
	if _, err := io.ReadFull(br, hdr); err != nil { // version length
		return
	}
	vBuf := make([]byte, binary.BigEndian.Uint32(hdr))
	if _, err := io.ReadFull(br, vBuf); err != nil {
		return
	}
	s.send("178", "20260610 10:00:00 EST")

	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(hdr))
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		msg := strings.Split(strings.TrimSuffix(string(body), "\x00"), "\x00")
		if len(msg) == 0 {
			continue
		}
		if msg[0] == "71" { // startApi
			s.send("9", "1", "1")          // nextValidId
			s.send("15", "1", "DU123456")  // managedAccounts
			continue
		}
		if s.script != nil {
			s.script(s, msg)
		}
	}
}

func guardConnect(t *testing.T, srv *guardServer) *IB {
	t.Helper()
	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ib.Connect(ctx, "127.0.0.1", srv.port(), 1); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return ib
}

// raceSink prevents field reads from being optimized away.
var raceSink float64

// TestGuardConcurrentRequestFlow drives many request-response cycles from
// concurrent user goroutines while the reader goroutine completes them.
// Guards: StartReq / AppendResult / EndReq must share one lock discipline
// for the Results map (a torn map write panics the whole process).
func TestGuardConcurrentRequestFlow(t *testing.T) {
	srv := newGuardServer(t, func(s *guardServer, msg []string) {
		if msg[0] == "62" { // reqAccountSummary: 62, version, reqId, group, tags
			reqID := msg[2]
			s.send("63", "1", reqID, "DU123456", "NetLiquidation", "100000.00", "USD")
			s.send("64", "1", reqID)
		}
	})
	defer srv.close()

	ib := guardConnect(t, srv)
	defer ib.Disconnect()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const goroutines = 6
	const perGoroutine = 8
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perGoroutine)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				values, err := ib.ReqAccountSummary(ctx, "All", "NetLiquidation")
				if err != nil {
					errs <- err
					return
				}
				if len(values) != 1 {
					errs <- fmt.Errorf("got %d values, want 1", len(values))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent request failed: %v", err)
	}
}

// TestGuardConcurrentReqPositions issues overlapping ReqPositions calls.
// IB streams one set of positions and one positionEnd per wire request;
// every concurrent caller must complete — none may hang until its context
// expires or return before data arrives.
func TestGuardConcurrentReqPositions(t *testing.T) {
	srv := newGuardServer(t, func(s *guardServer, msg []string) {
		if msg[0] == "61" { // reqPositions (outbound)
			s.send("61", "3", "DU123456",
				"265598", "AAPL", "STK", "", "0", "", "", "NASDAQ", "USD", "AAPL", "NMS",
				"100", "150.5")
			s.send("62", "1") // positionEnd
		}
	})
	defer srv.close()

	ib := guardConnect(t, srv)
	defer ib.Disconnect()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	const callers = 4
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			positions, err := ib.ReqPositions(ctx)
			if err != nil {
				errs <- fmt.Errorf("ReqPositions: %w", err)
				return
			}
			if len(positions) != 1 {
				errs <- fmt.Errorf("got %d positions, want 1", len(positions))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestGuardConcurrentReqCompletedOrders is the same guard for the other
// synthetic-key request. The server answers with completedOrdersEnd only;
// both callers must return (empty) instead of hanging.
func TestGuardConcurrentReqCompletedOrders(t *testing.T) {
	srv := newGuardServer(t, func(s *guardServer, msg []string) {
		if msg[0] == "99" { // reqCompletedOrders (outbound)
			s.send("102") // completedOrdersEnd
		}
	})
	defer srv.close()

	ib := guardConnect(t, srv)
	defer ib.Disconnect()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	const callers = 3
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ib.ReqCompletedOrders(ctx, false); err != nil {
				errs <- fmt.Errorf("ReqCompletedOrders: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestGuardTickerStreamCrossGoroutineRead streams ticks from the server while
// a subscriber goroutine reads ticker fields, which is the usage shown in the
// README (subscribe to UpdateEvent, then read ticker.Bid/Ask/Last).
// Guards: ticker mutations by the reader goroutine must be synchronized with
// user-goroutine reads.
func TestGuardTickerStreamCrossGoroutineRead(t *testing.T) {
	srv := newGuardServer(t, func(s *guardServer, msg []string) {
		if msg[0] == "1" { // reqMktData: 1, version, reqId, contract...
			reqID := msg[2]
			go func() {
				for i := 0; i < 300; i++ {
					px := fmt.Sprintf("%.2f", 150.0+float64(i%20)*0.01)
					if !s.send("1", "6", reqID, "1", px, "100", "0") { // bid
						return
					}
					if !s.send("1", "6", reqID, "2", px, "120", "0") { // ask
						return
					}
					if !s.send("2", "6", reqID, "5", "50") { // lastSize
						return
					}
					select {
					case <-s.done:
						return
					default:
					}
					time.Sleep(time.Millisecond)
				}
			}()
		}
	})
	defer srv.close()

	ib := guardConnect(t, srv)
	defer ib.Disconnect()

	con := contract.Stock("AAPL", "SMART", "USD")
	ticker, err := ib.ReqMktData(con, "", false, false)
	if err != nil {
		t.Fatalf("ReqMktData: %v", err)
	}

	ch, subID := ticker.UpdateEvent.Chan(512)
	defer ticker.UpdateEvent.Unsubscribe(subID)

	received := 0
	deadline := time.After(3 * time.Second)
	for received < 30 {
		select {
		case <-ch:
			received++
			// Cross-goroutine read of streaming fields — per the Ticker
			// concurrency contract this requires the read lock.
			ticker.RLock()
			raceSink = ticker.Bid + ticker.Ask + ticker.Last + float64(len(ticker.Ticks))
			ticker.RUnlock()
		case <-deadline:
			t.Fatalf("timed out after %d ticker updates", received)
		}
	}
	ib.CancelMktData(con)
}

// TestGuardDisconnectDuringTickStream disconnects while ticks are mid-flight.
// Guards: state.Reset (user goroutine) vs pending-ticker bookkeeping on the
// reader goroutine.
func TestGuardDisconnectDuringTickStream(t *testing.T) {
	for i := 0; i < 3; i++ {
		srv := newGuardServer(t, func(s *guardServer, msg []string) {
			if msg[0] == "1" {
				reqID := msg[2]
				go func() {
					for {
						if !s.send("1", "6", reqID, "1", "150.10", "100", "0") {
							return
						}
						select {
						case <-s.done:
							return
						default:
						}
					}
				}()
			}
		})

		ib := guardConnect(t, srv)
		con := contract.Stock("AAPL", "SMART", "USD")
		if _, err := ib.ReqMktData(con, "", false, false); err != nil {
			t.Fatalf("ReqMktData: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		ib.Disconnect()
		srv.close()
	}
}

// TestGuardHeartbeatLastTimeRace runs the heartbeat prober while inbound
// messages continuously update state.LastTime on the reader goroutine.
// Guards: LastTime is read by the heartbeat goroutine and must be
// synchronized with the reader goroutine's writes.
func TestGuardHeartbeatLastTimeRace(t *testing.T) {
	srv := newGuardServer(t, func(s *guardServer, msg []string) {
		if msg[0] == "49" { // reqCurrentTime
			s.send("49", "1", strconv.FormatInt(time.Now().Unix(), 10))
		}
	})
	defer srv.close()

	ib := guardConnect(t, srv)
	defer ib.Disconnect()

	// Continuous inbound traffic so LastTime is written while heartbeat reads.
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		for {
			if !srv.send("6", "2", "NetLiquidation", "100000.00", "USD", "DU123456") {
				return
			}
			select {
			case <-srv.done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	hbDone := make(chan struct{})
	go ib.heartbeatLoop(ctx, ReconnectConfig{
		ProbeInterval: 25 * time.Millisecond,
		ProbeTimeout:  150 * time.Millisecond,
	}, hbDone)

	select {
	case <-hbDone:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat loop did not exit")
	}
	if !ib.IsConnected() {
		t.Fatal("heartbeat should not have disconnected: traffic was flowing")
	}
}
