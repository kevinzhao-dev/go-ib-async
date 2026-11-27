package protocol

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedServer accepts one connection, captures the handshake version
// string, replies with serverVersion and then runs script.
func scriptedServer(t *testing.T, serverVersion string, script func(c net.Conn, r *bufio.Reader)) (port int, handshake <-chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		l.Close()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		prefix := make([]byte, 4)
		if _, err := io.ReadFull(r, prefix); err != nil {
			return
		}
		v, err := readTestFrame(r)
		if err != nil {
			return
		}
		hs <- string(v)
		writeTestFrame(c, serverVersion+"\x0020261127 10:00:00 EST\x00")
		if script != nil {
			script(c, r)
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, hs
}

func readTestFrame(r *bufio.Reader) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint32(hdr))
	_, err := io.ReadFull(r, b)
	return b, err
}

func writeTestFrame(c net.Conn, payload string) {
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	copy(frame[4:], payload)
	c.Write(frame)
}

func readyScript(accounts string) func(c net.Conn, r *bufio.Reader) {
	return func(c net.Conn, r *bufio.Reader) {
		readTestFrame(r) // startApi
		writeTestFrame(c, "9\x001\x00500\x00")
		writeTestFrame(c, "15\x001\x00"+accounts+"\x00")
		time.Sleep(300 * time.Millisecond)
	}
}

func TestClientHandshakeAdvertisesVersionsAndCapabilities(t *testing.T) {
	port, hs := scriptedServer(t, "178", readyScript("DU1, DU2 ,"))
	c := NewClient()
	c.OptCapab = "+PACEAPI"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx, "127.0.0.1", port, 4); err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect()

	if got, want := <-hs, "v157..178 +PACEAPI"; got != want {
		t.Fatalf("handshake = %q, want %q", got, want)
	}
	if accts := c.Accounts(); len(accts) != 2 || accts[0] != "DU1" || accts[1] != "DU2" {
		t.Fatalf("accounts = %q", accts)
	}
	if id := c.GetReqID(); id != 500 {
		t.Fatalf("first request id = %d, want nextValidId 500", id)
	}
	if c.State() != Connected || !c.IsReady() || c.CurrentClientID() != 4 {
		t.Fatalf("state %v ready %v clientId %d", c.State(), c.IsReady(), c.CurrentClientID())
	}
	// Accounts returns a copy.
	c.Accounts()[0] = "mutated"
	if c.Accounts()[0] != "DU1" {
		t.Fatal("Accounts must return a copy")
	}
}

func TestClientRejectsOldOrInvalidServerVersion(t *testing.T) {
	for _, v := range []string{"150", "abc"} {
		port, _ := scriptedServer(t, v, func(c net.Conn, r *bufio.Reader) { time.Sleep(time.Second) })
		c := NewClient()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := c.Connect(ctx, "127.0.0.1", port, 1)
		cancel()
		if err == nil {
			c.Disconnect()
			t.Fatalf("server version %q: expected handshake failure", v)
		}
		c.Wait()
		if c.State() != Disconnected || c.IsConnected() {
			t.Fatalf("server version %q: state %v after failed handshake", v, c.State())
		}
		select {
		case <-c.Done():
		default:
			t.Fatalf("server version %q: Done not closed", v)
		}
	}
}

func TestClientDisconnectCallbacksAndReconnect(t *testing.T) {
	port, _ := scriptedServer(t, "178", func(c net.Conn, r *bufio.Reader) {
		readyScript("DU1")(c, r)
		// script returns → connection closed by server
	})
	c := NewClient()
	var disconnects atomic.Int32
	c.OnDisconnect = func(error) { disconnects.Add(1) }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx, "127.0.0.1", port, 1); err != nil {
		t.Fatal(err)
	}
	done := c.Done()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Done not closed after peer closed")
	}
	c.Wait()
	if disconnects.Load() != 1 || c.IsConnected() {
		t.Fatalf("disconnects=%d connected=%v", disconnects.Load(), c.IsConnected())
	}

	// A new Connect gets a fresh Done channel.
	port2, _ := scriptedServer(t, "178", readyScript("DU1"))
	if err := c.Connect(ctx, "127.0.0.1", port2, 1); err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect()
	select {
	case <-c.Done():
		t.Fatal("Done of the new connection must be open")
	default:
	}
}

func TestClientDataBatchCallbacksAndPanicRecovery(t *testing.T) {
	port, _ := scriptedServer(t, "178", func(c net.Conn, r *bufio.Reader) {
		readyScript("DU1")(c, r)
	})
	c := NewClient()
	var arrived, processed, handled atomic.Int32
	c.OnDataArrived = func() { arrived.Add(1) }
	c.OnDataProcessed = func() { processed.Add(1) }
	c.OnMessage = func(msgID int, r *FieldReader) {
		handled.Add(1)
		if msgID == InMsgManagedAccounts {
			panic("handler bug") // must not kill the reader
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx, "127.0.0.1", port, 1); err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect()
	deadline := time.Now().Add(2 * time.Second)
	for handled.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if handled.Load() != 2 {
		t.Fatalf("handled %d messages, want 2", handled.Load())
	}
	if !c.IsConnected() {
		t.Fatal("a panicking handler must not drop the connection")
	}
	for processed.Load() < arrived.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if arrived.Load() == 0 || arrived.Load() != processed.Load() {
		t.Fatalf("arrived=%d processed=%d", arrived.Load(), processed.Load())
	}
}

func TestReqIDSequence(t *testing.T) {
	c := NewClient()
	c.UpdateReqID(10)
	if id := c.GetReqID(); id != 10 {
		t.Fatalf("id = %d", id)
	}
	c.UpdateReqID(5) // never goes backwards
	if id := c.GetReqID(); id != 11 {
		t.Fatalf("id = %d, want 11", id)
	}
}

func TestConnectionErrors(t *testing.T) {
	var conn Connection
	if conn.Conn() != nil || conn.IsConnected() || conn.HasPending() {
		t.Fatal("zero Connection must be disconnected")
	}
	if !conn.LastRecv().IsZero() {
		t.Fatal("LastRecv must be zero before any message")
	}
	if err := conn.SendRaw([]byte("x")); err == nil {
		t.Fatal("SendRaw must fail when not connected")
	}
	if err := conn.SendMessage([]byte("x")); err == nil {
		t.Fatal("SendMessage must fail when not connected")
	}
	if _, err := conn.ReadMessage(); err == nil {
		t.Fatal("ReadMessage must fail when not connected")
	}
	if err := conn.Disconnect(); err != nil {
		t.Fatalf("Disconnect on idle connection: %v", err)
	}
}

func pipeConnection(t *testing.T) (*Connection, net.Conn) {
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	c := &Connection{conn: client, reader: bufio.NewReader(client)}
	return c, server
}

func TestConnectionFramingLimits(t *testing.T) {
	c, server := pipeConnection(t)
	go server.Write([]byte{0, 0, 0, 0})
	if _, err := c.ReadMessage(); err == nil || !strings.Contains(err.Error(), "zero-length") {
		t.Fatalf("zero-length frame: err = %v", err)
	}

	c, server = pipeConnection(t)
	go server.Write([]byte{0x7f, 0xff, 0xff, 0xff})
	if _, err := c.ReadMessage(); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized frame: err = %v", err)
	}

	c, server = pipeConnection(t)
	go writeTestFrame(server, "4\x002\x00-1\x00")
	f, err := c.ReadMessage()
	if err != nil || len(f) != 3 || f[2] != "-1" {
		t.Fatalf("ReadMessage = %q, %v", f, err)
	}
	if c.LastRecv().IsZero() || c.NumMsgRecv.Load() != 1 {
		t.Fatal("receive statistics not updated")
	}
	if c.Conn() == nil {
		t.Fatal("Conn() must return the socket")
	}
	if err := c.Connect(context.Background(), "127.0.0.1", 1); err == nil {
		t.Fatal("Connect on a connected Connection must fail")
	}
}

func TestFieldReaderEdgeCases(t *testing.T) {
	r := NewFieldReader([]string{"1", "2", "x", "Infinity", "true", "", "1e3"})
	if got := r.ReadIntList(2); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("ReadIntList = %v", got)
	}
	if v := r.ReadInt(); v != 0 {
		t.Fatalf("invalid int = %d, want 0", v)
	}
	if v := r.ReadFloat(); !math.IsInf(v, 1) {
		t.Fatalf("Infinity = %g", v)
	}
	if !r.ReadBool() {
		t.Fatal(`"true" must read as true`)
	}
	if r.ReadFloat() != 0 {
		t.Fatal("empty float must be 0")
	}
	if r.ReadFloat() != 1000 {
		t.Fatal("exponent float")
	}
	if r.HasMore() || r.ReadString() != "" || r.ReadInt() != 0 {
		t.Fatal("reading past the end must return zero values")
	}
	r.Skip(10)
	if r.Pos != len(r.Fields) {
		t.Fatalf("Skip past end: pos %d", r.Pos)
	}
}
