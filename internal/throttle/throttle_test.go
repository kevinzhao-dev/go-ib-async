package throttle

import (
	"sync"
	"testing"
	"time"
)

func TestLimiterAllowsUnderLimit(t *testing.T) {
	l := New(5, 1*time.Second)
	for range 5 {
		if !l.TryNow() {
			t.Fatal("should allow under limit")
		}
	}
}

func TestLimiterBlocksOverLimit(t *testing.T) {
	l := New(3, 1*time.Second)
	for range 3 {
		l.TryNow()
	}
	if l.TryNow() {
		t.Fatal("should block over limit")
	}
}

func TestLimiterDisabled(t *testing.T) {
	l := New(0, 1*time.Second)
	for range 100 {
		if !l.TryNow() {
			t.Fatal("disabled limiter should always allow")
		}
	}
	l.Wait() // should not block
}

func TestLimiterWaitRespects(t *testing.T) {
	l := New(2, 100*time.Millisecond)
	l.Wait()
	l.Wait()

	start := time.Now()
	l.Wait() // should wait ~100ms for first to expire
	elapsed := time.Since(start)

	if elapsed < 50*time.Millisecond {
		t.Fatalf("Wait should have blocked, only took %v", elapsed)
	}
}

func TestLimiterConcurrent(t *testing.T) {
	l := New(10, 500*time.Millisecond)
	var wg sync.WaitGroup

	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Wait()
		}()
	}
	wg.Wait()
}
