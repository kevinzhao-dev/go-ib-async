package event

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubscribeAndEmit(t *testing.T) {
	var e Event[int]
	var got int
	e.Subscribe(func(v int) { got = v })
	e.Emit(42)
	if got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
}

func TestMultipleSubscribers(t *testing.T) {
	var e Event[string]
	var results []string
	var mu sync.Mutex
	for i := 0; i < 3; i++ {
		e.Subscribe(func(v string) {
			mu.Lock()
			results = append(results, v)
			mu.Unlock()
		})
	}
	e.Emit("hello")
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 3 {
		t.Fatalf("expected 3 callbacks, got %d", len(results))
	}
	for _, r := range results {
		if r != "hello" {
			t.Fatalf("expected 'hello', got %q", r)
		}
	}
}

func TestUnsubscribe(t *testing.T) {
	var e Event[int]
	count := 0
	id := e.Subscribe(func(v int) { count++ })
	e.Emit(1)
	e.Unsubscribe(id)
	e.Emit(2)
	if count != 1 {
		t.Fatalf("expected 1 call after unsubscribe, got %d", count)
	}
}

func TestChanSubscription(t *testing.T) {
	var e Event[int]
	ch, id := e.Chan(10)
	defer e.Unsubscribe(id)

	e.Emit(1)
	e.Emit(2)

	v1 := <-ch
	v2 := <-ch
	if v1 != 1 || v2 != 2 {
		t.Fatalf("expected 1,2 got %d,%d", v1, v2)
	}
}

func TestChanDropsWhenFull(t *testing.T) {
	var e Event[int]
	ch, id := e.Chan(1)
	defer e.Unsubscribe(id)

	e.Emit(1) // fills buffer
	e.Emit(2) // should be dropped

	v := <-ch
	if v != 1 {
		t.Fatalf("expected 1, got %d", v)
	}

	select {
	case <-ch:
		t.Fatal("should not have received second value")
	default:
		// expected
	}
}

func TestOnce(t *testing.T) {
	var e Event[string]
	ch := e.Once()

	e.Emit("first")
	e.Emit("second") // should not be received

	v := <-ch
	if v != "first" {
		t.Fatalf("expected 'first', got %q", v)
	}

	// Channel should be closed
	_, ok := <-ch
	if ok {
		t.Fatal("expected closed channel")
	}

	// Wait for the async unsubscribe goroutine to complete
	time.Sleep(10 * time.Millisecond)

	if e.Len() != 0 {
		t.Fatalf("expected 0 subscribers after Once, got %d", e.Len())
	}
}

func TestLen(t *testing.T) {
	var e Event[int]
	if e.Len() != 0 {
		t.Fatal("expected 0")
	}
	id1 := e.Subscribe(func(int) {})
	_, id2 := e.Chan(1)
	if e.Len() != 2 {
		t.Fatalf("expected 2, got %d", e.Len())
	}
	e.Unsubscribe(id1)
	e.Unsubscribe(id2)
	if e.Len() != 0 {
		t.Fatalf("expected 0, got %d", e.Len())
	}
}

func TestConcurrentEmit(t *testing.T) {
	var e Event[int]
	var total atomic.Int64

	for i := 0; i < 10; i++ {
		e.Subscribe(func(v int) {
			total.Add(int64(v))
		})
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Emit(1)
		}()
	}
	wg.Wait()

	if total.Load() != 1000 {
		t.Fatalf("expected 1000, got %d", total.Load())
	}
}
