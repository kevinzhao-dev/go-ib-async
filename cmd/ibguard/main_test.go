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
// order-mutating message kills the session and never reaches upstream.
func TestBlocksOrderMessages(t *testing.T) {
	for _, id := range []string{"3", "4", "15", "19", "21", "58"} {
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
