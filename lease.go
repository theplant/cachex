package cachex

import (
	"context"
	"errors"
	"runtime/debug"
	"slices"
	"time"
)

// Leaser keeps an old answer out of a layer that several instances share. A
// fetch leases the keys it misses in the layer, before it reads further down;
// a write of a key (Set, SetMany, Del, DelMany on the backend) ends its lease;
// and the fetch backfills the layer only with the keys whose lease it still
// holds. So an answer read before another instance's write is never written
// over it. A Cache never waits for a lease another instance holds: it fetches
// the key anyway, without backfilling that layer.
//
// Like Backend's Many methods, its methods are best effort: a *BatchError
// returned as is fails only the keys it lists; any other error fails every key.
type Leaser[T any] interface {
	// Lease takes a lease, for ttl, on each of keys that nobody leases, and
	// returns a token for each lease it took. A key leased by someone else is
	// left out, as is a key that failed.
	Lease(ctx context.Context, keys []string, ttl time.Duration) (map[string]string, error)
	// PutLeased writes, for each key of tokens whose lease still holds its
	// token, the key's entry in entries, or deletes the key if it has none, and
	// ends its lease. A key whose lease has ended or holds another token is
	// left alone, without an error.
	PutLeased(ctx context.Context, tokens map[string]string, entries map[string]Entry[T]) error
	// Release ends, for each key of tokens, its lease if it still holds the
	// token, and writes nothing.
	Release(ctx context.Context, tokens map[string]string) error
}

// Leasing is a Backend that can lease its keys. Its Leaser returns nil when
// leases are off; when they are on, every write of a key ends its lease.
// Wrappers of a backend forward it.
type Leasing[T any] interface {
	Leaser() Leaser[T]
}

// ByteLeaser is Leaser for a ByteBackend; Encoded turns it into a Leaser.
// Like ByteBackend.Set, PutLeased gets only entries that expire in the future.
type ByteLeaser interface {
	Lease(ctx context.Context, keys []string, ttl time.Duration) (map[string]string, error)
	PutLeased(ctx context.Context, tokens map[string]string, entries map[string]ByteEntry) error
	Release(ctx context.Context, tokens map[string]string) error
}

// ByteLeasing is a ByteBackend that can lease its keys; see Leasing.
type ByteLeasing interface {
	Leaser() ByteLeaser
}

// leaserOf returns b's Leaser, or nil if it does not lease.
func leaserOf[T any](b Backend[T]) Leaser[T] {
	if l, ok := b.(Leasing[T]); ok {
		return l.Leaser()
	}
	return nil
}

func (e *encoded[T]) Leaser() Leaser[T] {
	l, ok := e.b.(ByteLeasing)
	if !ok {
		return nil
	}
	bl := l.Leaser()
	if bl == nil {
		return nil
	}
	return encodedLeaser[T]{e: e, b: bl}
}

type encodedLeaser[T any] struct {
	e *encoded[T]
	b ByteLeaser
}

func (l encodedLeaser[T]) Lease(ctx context.Context, keys []string, ttl time.Duration) (map[string]string, error) {
	return l.b.Lease(ctx, keys, ttl)
}

// PutLeased encodes entries as Encoded's SetMany does: an entry already past
// its ExpiresAt (by the wall clock) deletes its key instead.
func (l encodedLeaser[T]) PutLeased(ctx context.Context, tokens map[string]string, entries map[string]Entry[T]) error {
	errs := map[string]error{}
	now := time.Now()
	live := make(map[string]ByteEntry, len(entries))
	for key, entry := range entries {
		if _, leased := tokens[key]; !leased || !entry.ExpiresAt.After(now) {
			continue
		}
		data, err := encodeEntry(l.e.codec, entry)
		if err != nil {
			errs[key] = err
			continue
		}
		live[key] = ByteEntry{Data: data, ExpiresAt: entry.ExpiresAt}
	}
	// an entry that does not encode has none in live: its key is deleted
	// under the lease, which ends it, rather than left leased until the TTL
	err := l.b.PutLeased(ctx, tokens, live)
	for key := range tokens {
		if kerr := errForKey(err, key); kerr != nil {
			errs[key] = kerr
		}
	}
	return batchError(errs)
}

func (l encodedLeaser[T]) Release(ctx context.Context, tokens map[string]string) error {
	return l.b.Release(ctx, tokens)
}

// errLeased answers a refresh that skipped the source: another instance holds
// the lease of a layer it missed, and refreshes the key itself.
var errLeased = errors.New("cachex: another instance holds the lease")

// lease takes, for the flights idxs that missed layer li, a lease there, if
// the layer leases, before the fetch reads further down. A lease lasts as
// long as a fetch may (WithFetchTimeout). A key it cannot lease (another
// instance holds it, or the call failed) is not backfilled into the layer.
func (c *Cache[T]) lease(ctx context.Context, p *claimed[T], li int, idxs []int) {
	if c.leasers == nil || c.leasers[li] == nil || len(idxs) == 0 {
		return
	}
	l := c.leasers[li]
	lctx, cancel := context.WithTimeout(ctx, c.opts.fetchTimeout)
	tokens, err := l.Lease(lctx, keysAt(p.keys, idxs), c.opts.fetchTimeout)
	cancel()
	if err != nil {
		c.opts.logger.WarnContext(ctx, "cachex: lease failed", "layer", li, "error", err)
	}
	for _, i := range idxs {
		k := &p.ks[i]
		if k.leases == nil {
			k.leases = &keyLeases{tokens: make([]string, len(c.layers))}
		}
		if token, ok := tokens[p.keys[i]]; ok {
			k.leases.tokens[li] = token
		} else {
			k.leases.refused = true
		}
	}
}

// keyLeases are the leases a fetch holds for a key, by layer ("": none), and
// whether a layer it missed refused it one. Taken only from a layer that
// leases: a fetch through layers that do not keeps a nil pointer, so
// claimedKey stays small.
type keyLeases struct {
	tokens  []string
	refused bool
}

// skipLeased answers, for a refresh, the flights idxs that another instance
// leases somewhere with errLeased, and returns the others.
func (p *claimed[T]) skipLeased(idxs []int) []int {
	if !p.refresh || p.c.leasers == nil {
		return idxs
	}
	var zero T
	return slices.DeleteFunc(idxs, func(i int) bool {
		if l := p.ks[i].leases; l != nil && l.refused {
			p.publish(i, zero, errLeased)
			return true
		}
		return false
	})
}

// release ends the leases the flights idxs still hold: a backfill used the
// others.
func (p *claimed[T]) release(ctx context.Context, idxs []int) {
	for li, leaser := range p.c.leasers {
		if leaser == nil {
			continue
		}
		var tokens map[string]string
		for _, i := range idxs {
			if l := p.ks[i].leases; l != nil && l.tokens[li] != "" {
				if tokens == nil {
					tokens = map[string]string{}
				}
				tokens[p.keys[i]] = l.tokens[li]
				l.tokens[li] = ""
			}
		}
		if tokens == nil {
			continue
		}
		p.c.releaseLeases(ctx, li, leaser, tokens)
	}
}

// releaseLeases ends tokens' leases in layer li. It runs after the fetch's
// panics are recovered (see claimed.run), so it recovers its own.
func (c *Cache[T]) releaseLeases(ctx context.Context, li int, leaser Leaser[T], tokens map[string]string) {
	rctx, cancel := context.WithTimeout(ctx, c.opts.fetchTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			c.opts.logger.ErrorContext(rctx, "cachex: panic during lease release", "layer", li, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if err := leaser.Release(rctx, tokens); err != nil {
		c.opts.logger.WarnContext(rctx, "cachex: lease release failed", "layer", li, "error", err)
	}
}

// putLeased backfills answers into layer j, which leases: only the keys whose
// lease the fetch holds there (in tokens, indexed by layer), each ending it.
func (c *Cache[T]) putLeased(ctx context.Context, j int, fills []fill[T], idxs []int, now time.Time) {
	tokens := map[string]string{}
	entries := map[string]Entry[T]{}
	for _, i := range idxs {
		f := &fills[i]
		if f.leases == nil || f.leases.tokens[j] == "" {
			continue
		}
		tokens[f.key] = f.leases.tokens[j]
		f.leases.tokens[j] = ""
		if e, ok := c.layers[j].place(f.entry, now); ok {
			entries[f.key] = e
		}
	}
	if len(tokens) == 0 {
		return
	}
	if err := c.leasers[j].PutLeased(ctx, tokens, entries); err != nil {
		c.opts.logger.WarnContext(ctx, "cachex: backfill failed", "layer", j, "error", err)
	}
}

// putLeasedOne is putLeased for one fill, out of backfillOne so that its
// frame does not grow the fetch goroutine's stack when no layer leases.
//
//go:noinline
func (c *Cache[T]) putLeasedOne(ctx context.Context, j int, f *fill[T], now time.Time) {
	c.putLeased(ctx, j, []fill[T]{*f}, []int{0}, now)
}
