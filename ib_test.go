package ibgo

import (
	"context"
	"encoding/binary"
	"net"
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
