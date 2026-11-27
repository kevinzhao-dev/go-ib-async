package ibgo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// runReconnect runs ConnectWithReconnect against the fake until the test
// ends and returns a channel that receives each successful connection.
func runReconnect(t *testing.T, ib *IB, port, clientID, fallbacks int) <-chan struct{} {
	t.Helper()
	cfg := DefaultReconnectConfig()
	cfg.RetryDelay = 20 * time.Millisecond
	cfg.ProbeInterval = 0
	cfg.SyncOrders = false
	cfg.ClientIDFallbacks = fallbacks
	ctx, cancel := context.WithCancel(context.Background())
	connected := make(chan struct{}, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ib.ConnectWithReconnect(ctx, "127.0.0.1", port, clientID, cfg, func() { connected <- struct{}{} })
	}()
	t.Cleanup(func() { cancel(); <-done })
	return connected
}

func TestConnectWithReconnectFallsBackToFreeClientID(t *testing.T) {
	srv := startFake(t)
	srv.SetClientIDsInUse(7, 8)
	ib := New()
	connected := runReconnect(t, ib, srv.Port(), 7, 3)
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("never connected; last error %v", ib.LastConnectError())
	}
	if ib.ClientID() != 9 || !ib.IsConnected() || ib.LastConnectError() != nil {
		t.Fatalf("client id %d connected %v error %v", ib.ClientID(), ib.IsConnected(), ib.LastConnectError())
	}
	if got := srv.ClientIDAttempts(); !slices.Equal(got, []int64{7, 8, 9}) {
		t.Fatalf("attempts = %v, want 7, 8, 9 without waiting between them", got)
	}
}

func TestConnectWithReconnectReportsClientIDInUse(t *testing.T) {
	srv := startFake(t)
	srv.SetClientIDsInUse(7)
	ib := New()
	connected := runReconnect(t, ib, srv.Port(), 7, 0)
	waitFor(t, "a failed attempt", func() bool { return ib.LastConnectError() != nil })
	err := ib.LastConnectError()
	var herr *HandshakeError
	if !errors.Is(err, ErrClientIDInUse) || !errors.As(err, &herr) || herr.ClientID != 7 {
		t.Fatalf("err = %v", err)
	}
	if ib.IsConnected() {
		t.Fatal("IsConnected must be false while TWS refuses the client id")
	}
	if got := srv.ClientIDAttempts(); slices.Contains(got, 8) {
		t.Fatalf("attempts = %v: no fallback without ClientIDFallbacks", got)
	}

	// The other client goes away: the next attempt connects and clears it.
	srv.SetClientIDsInUse()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("never connected after the client id became free")
	}
	if ib.ClientID() != 7 || ib.LastConnectError() != nil {
		t.Fatalf("client id %d error %v", ib.ClientID(), ib.LastConnectError())
	}
}

func TestConnectWithReconnectAllFallbacksInUse(t *testing.T) {
	srv := startFake(t)
	srv.SetClientIDsInUse(7, 8)
	ib := New()
	runReconnect(t, ib, srv.Port(), 7, 1)
	waitFor(t, "a failed attempt", func() bool { return ib.LastConnectError() != nil })
	err := ib.LastConnectError()
	if !errors.Is(err, ErrClientIDInUse) || !strings.Contains(err.Error(), "client ids 7-8 are all in use") {
		t.Fatalf("err = %v", err)
	}
}
