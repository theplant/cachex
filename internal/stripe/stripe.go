// Package stripe orders the writes and backfills of keys without a lock per
// key: keys hash into a fixed number of stripes, each with a read-write lock
// and two counters. See docs/design/write-order.md.
package stripe

import (
	"context"
	"hash/maphash"
	"sync"
	"sync/atomic"
)

// Count is the number of stripes in a Set.
//
// Keys share stripes by hash, so two keys in one stripe serialize their
// writes and a write to one can skip the other's backfill (a spare cache
// miss, never a stale value). 4096 stripes of 40 bytes cost 160 KB per Set.
const Count = 4096

// Stripe orders the writes and backfills of the keys that hash to it: a write
// holds it for writing, a backfill for reading.
type Stripe struct {
	mu    sync.RWMutex
	gen   atomic.Uint64 // writes so far
	fills atomic.Uint64 // backfills so far
}

// Generation counts the writes; a backfill compares it with the value it took
// before fetching to learn whether a write happened meanwhile.
func (s *Stripe) Generation() uint64 { return s.gen.Load() }

// AddWrite counts a write.
func (s *Stripe) AddWrite() { s.gen.Add(1) }

// AddFill counts a backfill that wrote the layer.
func (s *Stripe) AddFill() { s.fills.Add(1) }

// Epoch changes whenever the layer is written for a key of the stripe (a
// write or a backfill). If it has not changed since a request read the layer,
// re-reading it would find the same miss.
func (s *Stripe) Epoch() uint64 { return s.gen.Load() + s.fills.Load() }

// Lock takes the stripe for writing. A free stripe is taken even with a done
// ctx. Waiting (for another write, or a backfill writing the layer) gives up
// when ctx is done: Lock then returns ctx's error, and the lock it gave up on
// is still taken in the background, runs late while holding it, and is
// released.
func (s *Stripe) Lock(ctx context.Context, late func()) error {
	if s.mu.TryLock() {
		return nil
	}
	locked := make(chan struct{})
	go func() {
		s.mu.Lock()
		close(locked)
	}()
	select {
	case <-locked:
		return nil
	case <-ctx.Done():
		go func() {
			<-locked
			late()
			s.mu.Unlock()
		}()
		return ctx.Err()
	}
}

// Unlock releases a Lock.
func (s *Stripe) Unlock() { s.mu.Unlock() }

// TryLock takes the stripe for writing if it is free.
func (s *Stripe) TryLock() bool { return s.mu.TryLock() }

// TryRLock takes the stripe for reading if no write holds or awaits it; a
// backfill that cannot is skipped, so reads never wait on writes.
func (s *Stripe) TryRLock() bool { return s.mu.TryRLock() }

// RUnlock releases a TryRLock.
func (s *Stripe) RUnlock() { s.mu.RUnlock() }

// Set is Count stripes and the seed that maps keys to them. Each Set has its
// own random seed, so a key's stripe differs between Sets and processes.
type Set struct {
	seed    maphash.Seed
	stripes [Count]Stripe
}

// NewSet returns a Set with a random seed.
func NewSet() *Set { return &Set{seed: maphash.MakeSeed()} }

// For returns the stripe of key.
func (s *Set) For(key string) *Stripe {
	return &s.stripes[maphash.String(s.seed, key)%Count]
}
