// Package flight is a singleflight whose Claim answers at once whether the
// caller now leads the fetch of a key. Unlike golang.org/x/sync/singleflight,
// which decides that inside the goroutine it starts, this lets a batch read
// claim many keys, fetch the ones it leads together, and only then wait for
// the others, so two overlapping batches never wait on each other.
package flight

import (
	"sync"
	"sync/atomic"
)

// Flight is one in-flight fetch of a key.
type Flight[T any] struct {
	published atomic.Bool
	done      chan struct{}
	value     T
	err       error
}

// Done is closed once the result is published.
func (f *Flight[T]) Done() <-chan struct{} { return f.done }

// Result is the published result; read it only after Done is closed.
func (f *Flight[T]) Result() (T, error) { return f.value, f.err }

// Group registers the in-flight fetches by key. The zero value is ready to use.
type Group[K comparable, T any] struct {
	mu      sync.Mutex
	flights map[K]*Flight[T]
}

// Claim returns the key's flight and whether the caller leads it (it was just
// created). The leader must Publish and then Forget the flight; everyone else
// waits on Done.
func (g *Group[K, T]) Claim(key K) (*Flight[T], bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.flights[key]; ok {
		return f, false
	}
	if g.flights == nil {
		g.flights = map[K]*Flight[T]{}
	}
	f := &Flight[T]{done: make(chan struct{})}
	g.flights[key] = f
	return f, true
}

// Publish answers f's waiters, once: a flight has exactly one result, and
// later calls do nothing. f stays registered, so a caller that claims key
// until Forget joins f and gets its result at once.
func (g *Group[K, T]) Publish(f *Flight[T], value T, err error) {
	if !f.published.CompareAndSwap(false, true) {
		return
	}
	f.value, f.err = value, err
	close(f.done)
}

// Forget unregisters f, if it is still the flight registered for key: a
// flight that was dropped does not unregister the one that replaced it.
func (g *Group[K, T]) Forget(key K, f *Flight[T]) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.flights[key] == f {
		delete(g.flights, key)
	}
}

// Drop unregisters whatever flights hold keys without publishing or
// interrupting them: their waiters still get their results, and the next
// Claim of a key starts a new flight.
func (g *Group[K, T]) Drop(keys ...K) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, key := range keys {
		delete(g.flights, key)
	}
}
