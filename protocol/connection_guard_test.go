package protocol

import (
	"net"
	"testing"
	"time"
)

// TestGuardWriteTimeoutOnStuckPeer writes against a peer that accepts but
// never reads. Once kernel buffers fill, the write must fail within the
// configured WriteTimeout and tear the connection down — without this, a
// wedged TWS blocks every sender (including the heartbeat probe) forever
// while they queue on the write mutex.
func TestGuardWriteTimeoutOnStuckPeer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		// Never read; close only when the test finishes.
		t.Cleanup(func() { conn.Close() })
	}()

	c := &Connection{WriteTimeout: 200 * time.Millisecond}
	if err := c.Connect("127.0.0.1", l.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect()

	payload := make([]byte, 256*1024)
	start := time.Now()
	var sendErr error
	for i := 0; i < 200; i++ { // 50 MB total — far beyond socket buffers
		if err := c.SendMessage(payload); err != nil {
			sendErr = err
			break
		}
	}
	elapsed := time.Since(start)

	if sendErr == nil {
		t.Fatal("writes never failed against a peer that never reads")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("write failure took %v; should be bounded by WriteTimeout", elapsed)
	}
	if c.IsConnected() {
		t.Fatal("connection should be torn down after a write failure")
	}
	if err := c.SendMessage([]byte("x")); err == nil {
		t.Fatal("sends after teardown should fail immediately")
	}
}
