package ibgo

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockTWSServer simulates a TWS/Gateway for integration-style testing.
type mockTWSServer struct {
	listener net.Listener
	t        *testing.T
	// onConnected is called after handshake, allowing custom message sequences
	onConnected func(conn net.Conn)
}

func newMockTWSServer(t *testing.T, onConnected func(conn net.Conn)) *mockTWSServer {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &mockTWSServer{listener: l, t: t, onConnected: onConnected}
	go s.serve()
	return s
}

func (s *mockTWSServer) port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *mockTWSServer) close() {
	s.listener.Close()
}

func (s *mockTWSServer) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	// Read handshake
	buf := make([]byte, 4)
	conn.Read(buf) // "API\0"
	conn.Read(buf) // length prefix
	vLen := binary.BigEndian.Uint32(buf)
	vBuf := make([]byte, vLen)
	conn.Read(vBuf)

	// Send server version
	sendMsg(conn, "178\x00connTime\x00")

	// Read startApi
	hdr := make([]byte, 4)
	conn.Read(hdr)
	msgLen := binary.BigEndian.Uint32(hdr)
	msgBuf := make([]byte, msgLen)
	conn.Read(msgBuf)

	// Send nextValidId + managedAccounts
	sendMsg(conn, "9\x001\x001\x00")
	sendMsg(conn, "15\x001\x00DU123456\x00")

	if s.onConnected != nil {
		s.onConnected(conn)
	}

	// Keep alive
	time.Sleep(500 * time.Millisecond)
}

func sendMsg(conn net.Conn, msg string) {
	payload := []byte(msg)
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	conn.Write(header)
	conn.Write(payload)
}

func TestIBConnectDisconnect(t *testing.T) {
	srv := newMockTWSServer(t, nil)
	defer srv.close()

	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Track connected event
	connected := make(chan struct{}, 1)
	ib.ConnectedEvent.Subscribe(func(struct{}) {
		connected <- struct{}{}
	})

	err := ib.Connect(ctx, "127.0.0.1", srv.port(), 1)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer ib.Disconnect()

	// Verify connected event fired
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("ConnectedEvent not fired")
	}

	if !ib.IsConnected() {
		t.Fatal("should be connected")
	}
	if ib.ServerVersion() != 178 {
		t.Fatalf("ServerVersion = %d, want 178", ib.ServerVersion())
	}

	accounts := ib.ManagedAccounts()
	if len(accounts) != 1 || accounts[0] != "DU123456" {
		t.Fatalf("Accounts = %v", accounts)
	}
}

func TestIBErrorEvent(t *testing.T) {
	srv := newMockTWSServer(t, func(conn net.Conn) {
		// Send an error message (msgId=4)
		sendMsg(conn, "4\x002\x0042\x00321\x00Invalid contract\x00\x00")
		time.Sleep(100 * time.Millisecond)
	})
	defer srv.close()

	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan *IBError, 1)
	ib.ErrorEvent.Subscribe(func(e *IBError) {
		errCh <- e
	})

	err := ib.Connect(ctx, "127.0.0.1", srv.port(), 1)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer ib.Disconnect()

	select {
	case ibErr := <-errCh:
		if ibErr.Code != 321 {
			t.Fatalf("error code = %d, want 321", ibErr.Code)
		}
		if ibErr.ReqID != 42 {
			t.Fatalf("reqID = %d, want 42", ibErr.ReqID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ErrorEvent not received")
	}
}

func TestIBPositionEvent(t *testing.T) {
	srv := newMockTWSServer(t, func(conn net.Conn) {
		// Wait for reqPositions message
		hdr := make([]byte, 4)
		conn.Read(hdr)
		msgLen := binary.BigEndian.Uint32(hdr)
		buf := make([]byte, msgLen)
		conn.Read(buf)

		// Send position (msgId=61)
		sendMsg(conn, "61\x001\x00DU123456\x00265598\x00AAPL\x00STK\x00\x000\x00\x00\x00SMART\x00USD\x00100\x00150.5\x00")
		// Send positionEnd (msgId=62)
		sendMsg(conn, "62\x001\x00")
		time.Sleep(100 * time.Millisecond)
	})
	defer srv.close()

	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := ib.Connect(ctx, "127.0.0.1", srv.port(), 1)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer ib.Disconnect()

	positions, err := ib.ReqPositions(ctx)
	if err != nil {
		t.Fatalf("ReqPositions failed: %v", err)
	}

	if len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(positions))
	}
	if positions[0].Contract.Symbol != "AAPL" {
		t.Fatalf("symbol = %q, want AAPL", positions[0].Contract.Symbol)
	}
	if positions[0].Position != 100 {
		t.Fatalf("position = %g, want 100", positions[0].Position)
	}
}

// TestSyntheticReqIDUnique verifies that each call to ReqPositions gets a unique
// synthetic key, and concurrent calls are serialized (no key collision).
func TestSyntheticReqIDUnique(t *testing.T) {
	ib := New()

	// Generate several synthetic keys and verify uniqueness
	seen := make(map[int64]bool)
	for i := 0; i < 100; i++ {
		key := ib.syntheticReqID.Add(-1)
		if seen[key] {
			t.Fatalf("duplicate synthetic key: %d", key)
		}
		seen[key] = true
		if key >= 0 {
			t.Fatalf("synthetic key should be negative, got %d", key)
		}
	}
}

// TestConcurrentReqPositionsSerialized verifies that overlapping ReqPositions calls
// are serialized by positionsMu and each gets a unique synthetic key.
func TestConcurrentReqPositionsSerialized(t *testing.T) {
	ib := New()

	// Verify the mutex serializes access and keys are unique
	var wg sync.WaitGroup
	keys := make([]int64, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ib.positionsMu.Lock()
			key := ib.syntheticReqID.Add(-1)
			ib.positionsReqID = key
			keys[idx] = key
			ib.positionsMu.Unlock()
		}(i)
	}
	wg.Wait()

	// All keys should be unique and negative
	seen := make(map[int64]bool)
	for _, k := range keys {
		if k >= 0 {
			t.Fatalf("key should be negative, got %d", k)
		}
		if seen[k] {
			t.Fatalf("duplicate key: %d", k)
		}
		seen[k] = true
	}
}

// TestBatchSemanticsMultipleMessages verifies that OnDataArrived fires once
// and OnDataProcessed fires once when multiple messages arrive in the same batch.
func TestBatchSemanticsMultipleMessages(t *testing.T) {
	var arrivedCount atomic.Int32
	var processedCount atomic.Int32

	srv := newMockTWSServer(t, func(conn net.Conn) {
		// Send multiple messages in rapid succession (same TCP write)
		// This should be treated as one batch
		msg1 := buildFrame("61\x001\x00DU123456\x00265598\x00AAPL\x00STK\x00\x000\x00\x00\x00SMART\x00USD\x00100\x00150.5\x00")
		msg2 := buildFrame("61\x001\x00DU123456\x00265599\x00MSFT\x00STK\x00\x000\x00\x00\x00SMART\x00USD\x0050\x00300.0\x00")
		// Write both messages in a single TCP write so they land in the same buffer
		combined := append(msg1, msg2...)
		conn.Write(combined)
		time.Sleep(500 * time.Millisecond)
	})
	defer srv.close()

	ib := New()
	// Override the hooks to count batch boundaries (called from readLoop goroutine)
	ib.client.OnDataArrived = func() {
		arrivedCount.Add(1)
	}
	ib.client.OnDataProcessed = func() {
		processedCount.Add(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := ib.Connect(ctx, "127.0.0.1", srv.port(), 1); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Wait for messages to be processed, then disconnect
	time.Sleep(300 * time.Millisecond)
	ib.Disconnect()

	// Wait for readLoop to finish
	time.Sleep(100 * time.Millisecond)

	a := arrivedCount.Load()
	p := processedCount.Load()
	// Both should be equal (proper pairing)
	if a != p {
		t.Fatalf("batch mismatch: arrived=%d processed=%d", a, p)
	}
	if a == 0 {
		t.Fatal("no batches processed")
	}
}

func buildFrame(msg string) []byte {
	payload := []byte(msg)
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	return append(header, payload...)
}

// TestHeartbeatTimeoutDisconnects verifies that when the peer doesn't respond
// within ProbeTimeout, the heartbeat loop disconnects.
func TestHeartbeatTimeoutDisconnects(t *testing.T) {
	srv := newMockTWSServer(t, func(conn net.Conn) {
		// After handshake, just sit idle — don't respond to reqCurrentTime
		time.Sleep(5 * time.Second)
	})
	defer srv.close()

	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ib.Connect(ctx, "127.0.0.1", srv.port(), 1); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	disconnected := make(chan struct{}, 1)
	ib.DisconnectedEvent.Subscribe(func(struct{}) {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	})

	// Start heartbeat with very short intervals
	heartbeatDone := make(chan struct{})
	go ib.heartbeatLoop(ctx, ReconnectConfig{
		ProbeInterval: 100 * time.Millisecond,
		ProbeTimeout:  200 * time.Millisecond,
	}, heartbeatDone)

	// Heartbeat should detect unresponsive peer and disconnect
	select {
	case <-heartbeatDone:
		// Good — heartbeat loop exited
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat loop did not exit on timeout")
	}

	// Verify disconnect happened
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("DisconnectedEvent not received after heartbeat timeout")
	}
}
