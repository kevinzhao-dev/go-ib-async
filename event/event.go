// Package event provides a generic typed event system for pub/sub.
package event

import "sync"

// Event is a typed pub/sub event that supports both callback and channel subscriptions.
type Event[T any] struct {
	mu        sync.RWMutex
	callbacks []callbackEntry[T]
	channels  []chanEntry[T]
	nextID    uint64
}

type callbackEntry[T any] struct {
	id uint64
	fn func(T)
}

type chanEntry[T any] struct {
	id uint64
	ch chan T
}

// Subscribe registers a callback to be called on each Emit.
// Returns a subscription ID that can be used to Unsubscribe.
func (e *Event[T]) Subscribe(fn func(T)) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextID++
	e.callbacks = append(e.callbacks, callbackEntry[T]{id: e.nextID, fn: fn})
	return e.nextID
}

// Unsubscribe removes a callback or channel subscription by ID.
func (e *Event[T]) Unsubscribe(id uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, cb := range e.callbacks {
		if cb.id == id {
			e.callbacks = append(e.callbacks[:i], e.callbacks[i+1:]...)
			return
		}
	}
	for i, ch := range e.channels {
		if ch.id == id {
			close(ch.ch)
			e.channels = append(e.channels[:i], e.channels[i+1:]...)
			return
		}
	}
}

// Chan creates a channel subscription with the given buffer size.
// The returned channel receives values on each Emit.
// Call Unsubscribe with the returned ID to close the channel and unregister.
func (e *Event[T]) Chan(bufSize int) (<-chan T, uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextID++
	ch := make(chan T, bufSize)
	e.channels = append(e.channels, chanEntry[T]{id: e.nextID, ch: ch})
	return ch, e.nextID
}

// Once returns a channel that receives exactly one emission, then is closed.
func (e *Event[T]) Once() <-chan T {
	ch := make(chan T, 1)
	var once sync.Once
	var subID uint64
	subID = e.Subscribe(func(val T) {
		once.Do(func() {
			ch <- val
			close(ch)
			// Unsubscribe in a goroutine to avoid deadlock (Emit holds RLock).
			go e.Unsubscribe(subID)
		})
	})
	return ch
}

// Emit sends a value to all subscribers (callbacks first, then channels).
// Callbacks are called synchronously. Channel sends are non-blocking (drops if full).
func (e *Event[T]) Emit(val T) {
	e.mu.RLock()
	cbs := make([]callbackEntry[T], len(e.callbacks))
	copy(cbs, e.callbacks)
	chs := make([]chanEntry[T], len(e.channels))
	copy(chs, e.channels)
	e.mu.RUnlock()

	for _, cb := range cbs {
		cb.fn(val)
	}
	for _, ch := range chs {
		select {
		case ch.ch <- val:
		default:
			// drop if channel is full
		}
	}
}

// Len returns the total number of active subscriptions.
func (e *Event[T]) Len() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.callbacks) + len(e.channels)
}
