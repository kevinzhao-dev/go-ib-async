package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func frame(p string) []byte {
	b := make([]byte, 4+len(p))
	binary.BigEndian.PutUint32(b, uint32(len(p)))
	copy(b[4:], p)
	return b
}

// TestBlocksOrderMessages: allowed messages are forwarded, the first
// order-mutating message (or reqOpenOrders, here sent before START_API)
// kills the session and never reaches upstream.
func TestBlocksOrderMessages(t *testing.T) {
	for _, id := range []string{"3", "4", "15", "19", "21", "58", "5"} {
		t.Run("msg"+id, func(t *testing.T) {
			up, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer up.Close()
			*upstream = up.Addr().String()
			got := make(chan []byte, 1)
			go func() {
				c, err := up.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				b, _ := io.ReadAll(c)
				got <- b
			}()

			client, guardSide := net.Pipe()
			go handle(1, guardSide)
			v := "v157..178"
			hs := append([]byte("API\x00"), frame(v)[:4]...)
			hs = append(hs, v...)
			if _, err := client.Write(hs); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Write(frame("49\x001\x00")); err != nil { // reqCurrentTime
				t.Fatal(err)
			}
			_, _ = client.Write(frame(id + "\x0042\x00SECRET\x00"))
			_, _ = client.Write(frame("49\x001\x00"))
			client.Close()

			select {
			case b := <-got:
				if bytes.Contains(b, []byte("SECRET")) {
					t.Fatalf("blocked message %s was forwarded", id)
				}
				if !bytes.Contains(b, []byte("49\x001\x00")) || !bytes.HasPrefix(b, []byte("API\x00")) {
					t.Fatalf("allowed traffic not forwarded: %q", b)
				}
				if bytes.Count(b, []byte("49\x001\x00")) != 1 {
					t.Fatal("session should be killed at the blocked message")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("upstream session did not end")
			}
		})
	}
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		frame string
		pass  bool
	}{
		{"49\x001\x00", true},
		{"1\x0011\x00", true},
		{"104\x0024\x00", true},
		{"16\x001\x00", true}, // reqAllOpenOrders
		{"5\x001\x00", true},  // reqOpenOrders from client 99
		{"3\x0042\x00", false},
		{"03\x0042\x00", false},     // TWS parses the id as a number
		{"+3\x0042\x00", false},     // not a plain decimal id
		{" 3\x0042\x00", false},     // not a plain decimal id
		{"\x00\x00\x00\x03", false}, // binary message id (server version 201+)
		{"203\x00", false},          // protobuf placeOrder
		{"105\x00", false},          // newer than maxServerVersion
		{"26\x00", false},           // unassigned
		{"60\x00", false},           // unassigned
		{"0\x00", false},
		{"49", false},                    // no field terminator
		{"71\x002\x0099\x00\x00", false}, // second START_API
	} {
		s := newSession()
		s.clientID = 99
		if got := s.check([]byte(tc.frame)) == ""; got != tc.pass {
			t.Errorf("check(%q) pass = %v, want %v", tc.frame, got, tc.pass)
		}
	}
}

// reqOpenOrders binds manual TWS orders for client id 0: it is blocked for
// client 0 and before START_API, and passes for other client ids.
func TestReqOpenOrdersNeedsNonZeroClient(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start string // START_API frame, "" = none
		pass  bool
	}{
		{"no START_API", "", false},
		{"client 0", "71\x002\x000\x00\x00", false},
		{"client 7", "71\x002\x007\x00\x00", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession()
			if tc.start != "" {
				if why := s.check([]byte(tc.start)); why != "" {
					t.Fatalf("START_API blocked: %s", why)
				}
			}
			if got := s.check([]byte("5\x001\x00")) == ""; got != tc.pass {
				t.Fatalf("reqOpenOrders pass = %v, want %v", got, tc.pass)
			}
		})
	}
	for _, bad := range []string{"71\x002\x00\x00", "71\x002\x00-1\x00\x00", "71\x002\x00x\x00\x00", "71\x002\x00"} {
		if why := newSession().check([]byte(bad)); why == "" {
			t.Errorf("malformed START_API %q passed", bad)
		}
	}
}

func TestCapHandshake(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"v157..178", "v157..178"},
		{"v100..187", "v100..178"},
		{"v100..210 +PACEAPI", "v100..178 +PACEAPI"},
		{"v170", "v170"},
		{"v190..210", ""}, // minimum above the cap
		{"v201", ""},
		{"API", ""},
		{"v..178", ""},
	} {
		got, err := capHandshake([]byte(tc.in))
		if tc.want == "" {
			if err == nil {
				t.Errorf("capHandshake(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil || string(got) != tc.want {
			t.Errorf("capHandshake(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

// TestSessionFailsClosed: a client offering a new API version is capped, and
// a frame with a binary message id kills the session without forwarding.
func TestSessionFailsClosed(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	*upstream = up.Addr().String()
	got := make(chan []byte, 1)
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- b
	}()

	client, guardSide := net.Pipe()
	go handle(1, guardSide)
	hs := append([]byte("API\x00"), frame("v100..210")...)
	if _, err := client.Write(hs); err != nil {
		t.Fatal(err)
	}
	_, _ = client.Write(frame("49\x001\x00"))
	_, _ = client.Write(frame("\x00\x00\x00\x03SECRET"))
	client.Close()

	select {
	case b := <-got:
		if bytes.Contains(b, []byte("SECRET")) {
			t.Fatal("binary-id message was forwarded")
		}
		if !bytes.HasPrefix(b, append([]byte("API\x00"), frame("v100..178")...)) {
			t.Fatalf("handshake not capped: %q", b)
		}
		if !bytes.Contains(b, []byte("49\x001\x00")) {
			t.Fatalf("allowed traffic not forwarded: %q", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream session did not end")
	}
}
