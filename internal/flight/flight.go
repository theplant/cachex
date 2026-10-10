// Package flight is a singleflight whose Claim answers at once whether the
// caller now leads the fetch of a key. Unlike golang.org/x/sync/singleflight,
// which decides that inside the goroutine it starts, this lets a batch read
// claim many keys, fetch the ones it leads together, and only then wait for
// the others, so two overlapping batches never wait on each other.
package flight

import "sync"

// Flight is one in-flight fetch of a key.
type Flight[T any] struct {
	done  chan struct{}
	value T
	err   error
}

// Done is closed once the result is published.
func (f *Flight[T]) Done() <-chan struct{} { return f.done }

// Result is the published result; read it only after Done is closed.
func (f *Flight[T]) Result() (T, error) { return f.value, f.err }

// Group registers the in-flight fetches by key. The zero value is ready to use.
type Group[T any] struct {
	mu      sync.Mutex
	flights map[string]*Flight[T]
}

// Claim returns the key's flight and whether the caller leads it (it was just
// created). The leader must call Finish exactly once; everyone else waits on
// Done.
func (g *Group[T]) Claim(key string) (*Flight[T], bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.flights[key]; ok {
		return f, false
	}
	if g.flights == nil {
		g.flights = map[string]*Flight[T]{}
	}
	f := &Flight[T]{done: make(chan struct{})}
	g.flights[key] = f
	return f, true
}

// Finish publishes f's result. It unregisters f first (if f is still the one
// registered for key), so a caller that sees the result and comes back starts
// a new flight instead of joining a finished one; and a flight that was
// dropped does not unregister the one that replaced it.
func (g *Group[T]) Finish(key string, f *Flight[T], value T, err error) {
	g.mu.Lock()
	if g.flights[key] == f {
		delete(g.flights, key)
	}
	g.mu.Unlock()
	f.value, f.err = value, err
	close(f.done)
}

// Drop unregisters whatever flight holds key without publishing or
// interrupting it: its waiters still get its result, and the next Claim
// starts a new flight.
func (g *Group[T]) Drop(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.flights, key)
}

// Len is the number of registered flights.
func (g *Group[T]) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.flights)
}
