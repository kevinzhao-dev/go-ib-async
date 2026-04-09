// Package throttle implements request rate limiting for the IB API.
package throttle

import (
	"sync"
	"time"
)

// Limiter enforces a maximum number of requests per time interval
// using a sliding window approach.
type Limiter struct {
	mu          sync.Mutex
	maxRequests int
	interval    time.Duration
	timestamps  []time.Time
}

// New creates a Limiter allowing maxRequests per interval.
// If maxRequests is 0, no throttling is applied.
func New(maxRequests int, interval time.Duration) *Limiter {
	return &Limiter{
		maxRequests: maxRequests,
		interval:    interval,
		timestamps:  make([]time.Time, 0, maxRequests),
	}
}

// Wait blocks until a request is allowed, then records it.
// Returns immediately if maxRequests is 0 (disabled).
func (l *Limiter) Wait() {
	if l.maxRequests == 0 {
		return
	}

	for {
		l.mu.Lock()
		now := time.Now()

		// Prune timestamps outside the window
		cutoff := now.Add(-l.interval)
		pruned := l.timestamps[:0]
		for _, ts := range l.timestamps {
			if ts.After(cutoff) {
				pruned = append(pruned, ts)
			}
		}
		l.timestamps = pruned

		if len(l.timestamps) < l.maxRequests {
			l.timestamps = append(l.timestamps, now)
			l.mu.Unlock()
			return
		}

		// Need to wait until the oldest timestamp expires
		waitUntil := l.timestamps[0].Add(l.interval)
		l.mu.Unlock()

		sleepDur := time.Until(waitUntil)
		if sleepDur > 0 {
			time.Sleep(sleepDur)
		}
	}
}

// TryNow attempts to record a request without waiting.
// Returns true if allowed, false if rate limit exceeded.
func (l *Limiter) TryNow() bool {
	if l.maxRequests == 0 {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.interval)
	pruned := l.timestamps[:0]
	for _, ts := range l.timestamps {
		if ts.After(cutoff) {
			pruned = append(pruned, ts)
		}
	}
	l.timestamps = pruned

	if len(l.timestamps) < l.maxRequests {
		l.timestamps = append(l.timestamps, now)
		return true
	}
	return false
}
