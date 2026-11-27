package ibgo

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/market"
)

func msftE2E() *contract.Contract {
	c := contract.Stock("MSFT", "SMART", "USD")
	c.ConID = 272093
	return c
}

// fakeSub is a market data subscription whose sends and cancels are
// recorded instead of going to the network.
type fakeSub struct {
	mu      sync.Mutex
	sends   []int64
	cancels []int64
	fail    atomic.Bool
	// block, when set, is called by every send after the first one.
	block func()
}

func (f *fakeSub) sub(con *contract.Contract) *subscription {
	return &subscription{
		kind: subMktData, con: con, ticker: market.NewTicker(con),
		send: func(reqID int64) error {
			f.mu.Lock()
			n := len(f.sends)
			f.mu.Unlock()
			if n > 0 && f.block != nil {
				f.block()
			}
			if f.fail.Load() {
				return errors.New("not connected")
			}
			f.mu.Lock()
			f.sends = append(f.sends, reqID)
			f.mu.Unlock()
			return nil
		},
		cancel: func(reqID int64) error {
			f.mu.Lock()
			f.cancels = append(f.cancels, reqID)
			f.mu.Unlock()
			return nil
		},
	}
}

func (f *fakeSub) counts() (sends, cancels []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.sends...), append([]int64(nil), f.cancels...)
}

func numSubs(ib *IB) int {
	ib.subsMu.Lock()
	defer ib.subsMu.Unlock()
	return len(ib.subs)
}

// A re-send that fails (the connection dropped again during the restore)
// keeps the subscription for the next reconnect.
func TestRestoreKeepsFailedSubscriptions(t *testing.T) {
	ib := newTestIB()
	ib.connEpoch.Store(1)
	f := &fakeSub{}
	if _, err := ib.startSub(f.sub(aaplE2E())); err != nil {
		t.Fatal(err)
	}

	ib.connEpoch.Add(1)
	f.fail.Store(true)
	ib.restoreSubscriptions()
	if n := numSubs(ib); n != 1 {
		t.Fatalf("%d subscriptions after a failed restore, want 1", n)
	}

	ib.connEpoch.Add(1)
	f.fail.Store(false)
	ib.restoreSubscriptions()
	sends, _ := f.counts()
	if len(sends) != 2 {
		t.Fatalf("sends = %v, want the initial send and one restore", sends)
	}

	ib.CancelMktData(aaplE2E())
	if _, cancels := f.counts(); len(cancels) != 1 || cancels[0] != sends[1] {
		t.Fatalf("cancels = %v, want the restored request id %d", cancels, sends[1])
	}
	if n := numSubs(ib); n != 0 {
		t.Fatalf("%d subscriptions left", n)
	}
}

// A subscription that was never re-sent on the current connection is
// unknown to TWS: cancelling it sends nothing.
func TestCancelStaleSubscriptionSendsNothing(t *testing.T) {
	ib := newTestIB()
	ib.connEpoch.Store(1)
	f := &fakeSub{}
	if _, err := ib.startSub(f.sub(aaplE2E())); err != nil {
		t.Fatal(err)
	}
	ib.connEpoch.Add(1) // reconnected, not restored yet
	ib.CancelMktData(aaplE2E())
	if _, cancels := f.counts(); len(cancels) != 0 {
		t.Fatalf("cancel sent for a request id of an old connection: %v", cancels)
	}
	if n := numSubs(ib); n != 0 {
		t.Fatalf("%d subscriptions left", n)
	}
}

// A cancel that arrives while the restore is running is not undone by it.
func TestCancelDuringRestoreSticks(t *testing.T) {
	ib := newTestIB()
	ib.connEpoch.Store(1)
	entered := make(chan struct{})
	release := make(chan struct{})
	a := &fakeSub{block: func() { close(entered); <-release }}
	b := &fakeSub{}
	if _, err := ib.startSub(a.sub(aaplE2E())); err != nil { // lower id: restored first
		t.Fatal(err)
	}
	if _, err := ib.startSub(b.sub(msftE2E())); err != nil {
		t.Fatal(err)
	}

	ib.connEpoch.Add(1)
	restored := make(chan struct{})
	go func() { ib.restoreSubscriptions(); close(restored) }()
	<-entered // the restore is re-sending a

	cancelled := make(chan bool, 1)
	go func() { cancelled <- ib.endSubByContract(subMktData, "", msftE2E()) }()
	time.Sleep(20 * time.Millisecond) // the cancel is now waiting for the restore
	close(release)
	<-restored
	if !<-cancelled {
		t.Fatal("cancel during the restore found no subscription")
	}

	// b is either never re-sent, or re-sent and then cancelled.
	sends, cancels := b.counts()
	if len(sends)-1 != len(cancels) {
		t.Fatalf("b still active at TWS: sends %v, cancels %v", sends, cancels)
	}
	if n := numSubs(ib); n != 1 {
		t.Fatalf("%d subscriptions left, want only a", n)
	}
}

// Subscriptions made on the current connection (here from a ConnectedEvent
// handler) are not re-sent by the restore; they are after a reconnect.
func TestRestoreSkipsSubscriptionsOfCurrentConnection(t *testing.T) {
	srv := startFake(t)
	srv.Handle(49, func([]string) [][]byte { return [][]byte{fakegw.Msg(49, 1, 1795753114)} })
	ib := New()
	var once sync.Once
	subErr := make(chan error, 1)
	ib.ConnectedEvent.Subscribe(func(struct{}) {
		once.Do(func() {
			_, err := ib.ReqMktData(aaplE2E(), "", false, false)
			subErr <- err
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := DefaultReconnectConfig()
	cfg.RetryDelay = 20 * time.Millisecond
	cfg.ProbeInterval = 0
	cfg.SyncOrders = false
	connected := make(chan struct{}, 4)
	go ib.ConnectWithReconnect(ctx, "127.0.0.1", srv.Port(), e2eClientID, cfg, func() { connected <- struct{}{} })
	<-connected
	if err := <-subErr; err != nil {
		t.Fatal(err)
	}

	// A round trip after the restore: a duplicate reqMktData would arrive
	// before the reqCurrentTime.
	roundTrip := func() {
		t.Helper()
		ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel2()
		if _, err := ib.ReqCurrentTime(ctx2); err != nil {
			t.Fatal(err)
		}
	}
	roundTrip()
	if n := srv.Received(1); n != 1 {
		t.Fatalf("reqMktData sent %d times on the first connection, want 1", n)
	}

	srv.DropConnection()
	<-connected
	roundTrip()
	if n := srv.Received(1); n != 2 {
		t.Fatalf("reqMktData sent %d times after the reconnect, want 2", n)
	}
}
