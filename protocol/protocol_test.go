package protocol

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
)

// --- Encoder Tests ---

func TestEncodeFieldString(t *testing.T) {
	if EncodeField("hello") != "hello" {
		t.Fatal("string encoding failed")
	}
}

func TestEncodeFieldNil(t *testing.T) {
	if EncodeField(nil) != "" {
		t.Fatal("nil should encode as empty")
	}
}

func TestEncodeFieldBool(t *testing.T) {
	if EncodeField(true) != "1" {
		t.Fatal("true should encode as 1")
	}
	if EncodeField(false) != "0" {
		t.Fatal("false should encode as 0")
	}
}

func TestEncodeFieldInt(t *testing.T) {
	if EncodeField(42) != "42" {
		t.Fatalf("got %q", EncodeField(42))
	}
}

func TestEncodeFieldInt64(t *testing.T) {
	if EncodeField(int64(100)) != "100" {
		t.Fatalf("got %q", EncodeField(int64(100)))
	}
}

func TestEncodeFieldInt64Unset(t *testing.T) {
	if EncodeField(int64(math.MaxInt32)) != "" {
		t.Fatal("UNSET_INTEGER should encode as empty")
	}
}

func TestEncodeFieldFloat64(t *testing.T) {
	if EncodeField(150.5) != "150.5" {
		t.Fatalf("got %q", EncodeField(150.5))
	}
}

func TestEncodeFieldFloat64Unset(t *testing.T) {
	if EncodeField(math.MaxFloat64) != "" {
		t.Fatal("UNSET_DOUBLE should encode as empty")
	}
}

func TestEncodeFieldFloat64Inf(t *testing.T) {
	if EncodeField(math.Inf(1)) != "Infinite" {
		t.Fatalf("got %q", EncodeField(math.Inf(1)))
	}
}

func TestEncodeFieldTagValues(t *testing.T) {
	tvs := []contract.TagValue{
		{Tag: "key1", Value: "val1"},
		{Tag: "key2", Value: "val2"},
	}
	got := EncodeField(tvs)
	if got != "key1=val1;key2=val2;" {
		t.Fatalf("got %q", got)
	}
}

func TestEncodeContract(t *testing.T) {
	c := contract.Stock("AAPL", "SMART", "USD")
	c.ConID = 265598
	encoded := EncodeContract(c)
	parts := strings.Split(encoded, "\x00")
	if len(parts) != 12 {
		t.Fatalf("expected 12 fields, got %d", len(parts))
	}
	if parts[0] != "265598" {
		t.Fatalf("conId = %q, want 265598", parts[0])
	}
	if parts[1] != "AAPL" {
		t.Fatalf("symbol = %q, want AAPL", parts[1])
	}
	if parts[2] != "STK" {
		t.Fatalf("secType = %q, want STK", parts[2])
	}
	if parts[7] != "SMART" {
		t.Fatalf("exchange = %q, want SMART", parts[7])
	}
	if parts[9] != "USD" {
		t.Fatalf("currency = %q, want USD", parts[9])
	}
}

func TestBuildMessage(t *testing.T) {
	msg := BuildMessage(1, 11, int64(42), "hello", true, false, nil)
	s := string(msg)
	parts := strings.Split(s, "\x00")
	// last element is empty due to trailing \x00
	parts = parts[:len(parts)-1]
	expected := []string{"1", "11", "42", "hello", "1", "0", ""}
	if len(parts) != len(expected) {
		t.Fatalf("got %d fields, want %d: %v", len(parts), len(expected), parts)
	}
	for i, want := range expected {
		if parts[i] != want {
			t.Fatalf("field[%d] = %q, want %q", i, parts[i], want)
		}
	}
}

// --- FieldReader Tests ---

func TestFieldReaderReadString(t *testing.T) {
	r := NewFieldReader([]string{"hello", "world"})
	if r.ReadString() != "hello" || r.ReadString() != "world" {
		t.Fatal("ReadString failed")
	}
	if r.ReadString() != "" {
		t.Fatal("reading past end should return empty")
	}
}

func TestFieldReaderReadInt(t *testing.T) {
	r := NewFieldReader([]string{"42", "", "-1"})
	if r.ReadInt() != 42 {
		t.Fatal("ReadInt failed")
	}
	if r.ReadInt() != 0 {
		t.Fatal("empty should return 0")
	}
	if r.ReadInt() != -1 {
		t.Fatal("negative int failed")
	}
}

func TestFieldReaderReadFloat(t *testing.T) {
	r := NewFieldReader([]string{"150.5", "", "Infinite"})
	if r.ReadFloat() != 150.5 {
		t.Fatal("ReadFloat failed")
	}
	if r.ReadFloat() != 0 {
		t.Fatal("empty should return 0")
	}
	if !math.IsInf(r.ReadFloat(), 1) {
		t.Fatal("Infinite should return +Inf")
	}
}

func TestFieldReaderReadBool(t *testing.T) {
	r := NewFieldReader([]string{"1", "0", "", "true"})
	if !r.ReadBool() {
		t.Fatal("1 should be true")
	}
	if r.ReadBool() {
		t.Fatal("0 should be false")
	}
	if r.ReadBool() {
		t.Fatal("empty should be false")
	}
	if !r.ReadBool() {
		t.Fatal("true should be true")
	}
}

func TestFieldReaderSkip(t *testing.T) {
	r := NewFieldReader([]string{"a", "b", "c", "d"})
	r.Skip(2)
	if r.ReadString() != "c" {
		t.Fatal("Skip failed")
	}
}

// --- Connection Tests ---

func TestConnectionSendReceive(t *testing.T) {
	// Create a TCP pipe
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	conn := &Connection{conn: client, reader: bufio.NewReader(client)}

	// Send a message in a goroutine
	go func() {
		payload := []byte("hello\x00world\x00")
		conn.SendMessage(payload)
	}()

	// Read from server side
	header := make([]byte, 4)
	server.Read(header)
	msgLen := binary.BigEndian.Uint32(header)
	body := make([]byte, msgLen)
	server.Read(body)

	if string(body) != "hello\x00world\x00" {
		t.Fatalf("got %q", string(body))
	}
}

func TestConnectionReadMessage(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	conn := &Connection{conn: client, reader: bufio.NewReader(client)}

	// Write a framed message from "server" side
	go func() {
		payload := []byte("9\x001\x0042\x00")
		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, uint32(len(payload)))
		server.Write(header)
		server.Write(payload)
	}()

	fields, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields[0] != "9" || fields[1] != "1" || fields[2] != "42" {
		t.Fatalf("unexpected fields: %v", fields)
	}
}

// --- Mock IB Server ---

type mockIBServer struct {
	listener net.Listener
	t        *testing.T
}

func newMockIBServer(t *testing.T) *mockIBServer {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &mockIBServer{listener: l, t: t}
	go s.serve()
	return s
}

func (s *mockIBServer) addr() string {
	return s.listener.Addr().String()
}

func (s *mockIBServer) port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *mockIBServer) close() {
	s.listener.Close()
}

func (s *mockIBServer) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	// Read handshake: "API\0" + 4-byte length + version string
	buf := make([]byte, 4)
	conn.Read(buf) // "API\0"
	if string(buf) != "API\x00" {
		s.t.Errorf("expected API\\0, got %q", buf)
		return
	}

	// Read length-prefixed version string
	conn.Read(buf) // 4-byte length
	vLen := binary.BigEndian.Uint32(buf)
	vBuf := make([]byte, vLen)
	conn.Read(vBuf)
	// vBuf should be something like "v157..178"

	// Send server version response
	sendFramed(conn, "178\x00connTime\x00")

	// Read startApi message
	header := make([]byte, 4)
	conn.Read(header)
	msgLen := binary.BigEndian.Uint32(header)
	msgBuf := make([]byte, msgLen)
	conn.Read(msgBuf)

	// Send nextValidId (msgId=9)
	sendFramed(conn, "9\x001\x001\x00")

	// Send managedAccounts (msgId=15)
	sendFramed(conn, "15\x001\x00DU123456\x00")

	// Keep connection alive briefly
	time.Sleep(200 * time.Millisecond)
}

func sendFramed(conn net.Conn, msg string) {
	payload := []byte(msg)
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	conn.Write(header)
	conn.Write(payload)
}

// --- Client Handshake Test ---

func TestClientHandshake(t *testing.T) {
	srv := newMockIBServer(t)
	defer srv.close()

	client := NewClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx, "127.0.0.1", srv.port(), 1)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Disconnect()

	if !client.IsConnected() {
		t.Fatal("should be connected")
	}
	if !client.IsReady() {
		t.Fatal("should be ready")
	}
	if client.ServerVersion != 178 {
		t.Fatalf("ServerVersion = %d, want 178", client.ServerVersion)
	}

	accounts := client.Accounts()
	if len(accounts) != 1 || accounts[0] != "DU123456" {
		t.Fatalf("Accounts = %v, want [DU123456]", accounts)
	}

	// Request ID should be at least 1
	id := client.GetReqID()
	if id < 1 {
		t.Fatalf("GetReqID() = %d, want >= 1", id)
	}
}

func TestClientConnectTimeout(t *testing.T) {
	// Listen but never accept
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // close immediately so connection is refused

	client := NewClient()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := client.Connect(ctx, "127.0.0.1", port, 1)
	if err == nil {
		client.Disconnect()
		t.Fatal("expected error")
	}
}

// --- Decoder Tests ---

func TestDecoderInterpret(t *testing.T) {
	var gotMsgID int
	var gotFields []string

	handler := func(msgID int, reader *FieldReader) {
		gotMsgID = msgID
		for reader.HasMore() {
			gotFields = append(gotFields, reader.ReadString())
		}
	}

	d := NewDecoder(178, handler)
	d.Interpret([]string{"9", "1", "42"})

	if gotMsgID != 9 {
		t.Fatalf("msgID = %d, want 9", gotMsgID)
	}
	if len(gotFields) != 2 || gotFields[0] != "1" || gotFields[1] != "42" {
		t.Fatalf("fields = %v, want [1 42]", gotFields)
	}
}

func TestDecoderPanicRecovery(t *testing.T) {
	handler := func(msgID int, reader *FieldReader) {
		panic("test panic")
	}

	d := NewDecoder(178, handler)
	// Should not panic
	d.Interpret([]string{"1", "data"})
}

func TestDecoderEmptyFields(t *testing.T) {
	handler := func(msgID int, reader *FieldReader) {
		t.Fatal("should not be called")
	}

	d := NewDecoder(178, handler)
	d.Interpret([]string{}) // should be a no-op
}

// --- Round-trip Tests ---

func TestEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		fields []interface{}
	}{
		{"simple ints", []interface{}{1, 11, int64(42)}},
		{"strings", []interface{}{"hello", "world"}},
		{"mixed", []interface{}{9, 1, int64(100), "AAPL", 150.5, true}},
		{"unset values", []interface{}{int64(math.MaxInt32), math.MaxFloat64}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := BuildMessage(tt.fields...)
			s := string(msg)
			parts := strings.Split(s, "\x00")
			if len(parts) > 0 && parts[len(parts)-1] == "" {
				parts = parts[:len(parts)-1]
			}

			r := NewFieldReader(parts)
			for i, f := range tt.fields {
				switch v := f.(type) {
				case int:
					got := r.ReadString()
					want := fmt.Sprintf("%d", v)
					if got != want {
						t.Fatalf("field[%d] = %q, want %q", i, got, want)
					}
				case int64:
					got := r.ReadString()
					want := EncodeField(v)
					if got != want {
						t.Fatalf("field[%d] = %q, want %q", i, got, want)
					}
				case float64:
					got := r.ReadString()
					want := EncodeField(v)
					if got != want {
						t.Fatalf("field[%d] = %q, want %q", i, got, want)
					}
				case bool:
					got := r.ReadBool()
					if got != v {
						t.Fatalf("field[%d] = %v, want %v", i, got, v)
					}
				case string:
					got := r.ReadString()
					if got != v {
						t.Fatalf("field[%d] = %q, want %q", i, got, v)
					}
				}
			}
		})
	}
}
