package cachex

import (
	"context"
	"math/rand/v2"
	"time"
)

// DefaultJitter is the jitter of a layer that does not set one (see Jitter).
const DefaultJitter = 0.1

// maxTTL bounds fresh plus stale, so an entry's times stay within what a
// time.Duration and the entry encoding (UnixNano, up to the year 2262) hold.
const maxTTL = 100 * 365 * 24 * time.Hour

// Layer is one level of a Cache: a backend and how long its entries stay
// fresh and usable. Build it with NewLayer.
type Layer[T any] struct {
	backend Backend[T]
	cfg     layerConfig
}

type layerConfig struct {
	fresh, stale     time.Duration
	nfFresh, nfStale time.Duration
	jitter           float64
}

// LayerOption configures a Layer.
type LayerOption func(*layerConfig)

// TTL sets how long a value stays fresh and, after that, how long it is still
// served stale while being refreshed in the background (zero: not at all).
// Both count from when an entry is written into the layer (an answer of the
// source, a copy from a layer below, a Set); a copy is never fresher or longer
// lived than the entry it came from. Required, with a positive fresh; fresh
// plus stale is at most 100 years.
func TTL(fresh, stale time.Duration) LayerOption {
	return func(c *layerConfig) { c.fresh, c.stale = fresh, stale }
}

// NotFoundTTL is TTL for the answer that a key does not exist. With a zero
// fresh (the default) the layer does not record such answers.
func NotFoundTTL(fresh, stale time.Duration) LayerOption {
	return func(c *layerConfig) { c.nfFresh, c.nfStale = fresh, stale }
}

// Jitter shortens each entry's fresh period by a random part of up to ratio
// (in [0, 1), default DefaultJitter), so entries written together do not all
// turn stale or rotten at the same moment. It never lengthens one.
func Jitter(ratio float64) LayerOption {
	return func(c *layerConfig) { c.jitter = ratio }
}

// NewLayer returns a layer storing its entries in backend.
func NewLayer[T any](backend Backend[T], opts ...LayerOption) Layer[T] {
	if backend == nil {
		panic("cachex: NewLayer: backend is required")
	}
	cfg := layerConfig{jitter: DefaultJitter}
	for _, opt := range opts {
		opt(&cfg)
	}
	switch {
	case cfg.fresh <= 0:
		panic("cachex: NewLayer: TTL with a positive fresh TTL is required")
	case cfg.stale < 0 || cfg.nfFresh < 0 || cfg.nfStale < 0:
		panic("cachex: NewLayer: TTLs must not be negative")
	case cfg.nfFresh == 0 && cfg.nfStale > 0:
		panic("cachex: NewLayer: a not-found stale TTL needs a positive not-found fresh TTL")
	case cfg.fresh > maxTTL-cfg.stale || cfg.nfFresh > maxTTL-cfg.nfStale:
		panic("cachex: NewLayer: fresh plus stale must be at most 100 years")
	case !(cfg.jitter >= 0 && cfg.jitter < 1): // also rejects NaN
		panic("cachex: NewLayer: jitter must be in [0, 1)")
	}
	return Layer[T]{backend: backend, cfg: cfg}
}

// place is what the layer stores, at now, for answer a: the source's answer
// (see Cache.answer) or an entry found in a layer below. The layer's TTLs
// count from now, and the entry is no fresher and no longer usable than a (a
// zero time in a is no bound). So an upper layer keeps a copy for its own TTL,
// but never past the entry below. It returns false if the layer does not keep
// a: a not-found without a not-found TTL, or an entry already rotten; the key
// is deleted there instead.
func (l *Layer[T]) place(a Entry[T], now time.Time) (Entry[T], bool) {
	fresh, stale := l.cfg.fresh, l.cfg.stale
	value := a.Value
	if a.NotFound {
		fresh, stale = l.cfg.nfFresh, l.cfg.nfStale
		if fresh == 0 {
			return Entry[T]{}, false
		}
		var zero T
		value = zero
	}
	if l.cfg.jitter > 0 {
		fresh -= time.Duration(rand.Float64() * l.cfg.jitter * float64(fresh))
	}
	e := Entry[T]{
		Value:      value,
		NotFound:   a.NotFound,
		CachedAt:   a.CachedAt,
		FreshUntil: now.Add(fresh),
		ExpiresAt:  now.Add(fresh + stale),
	}
	if !a.ExpiresAt.IsZero() && a.ExpiresAt.Before(e.ExpiresAt) {
		e.ExpiresAt = a.ExpiresAt
	}
	if !a.FreshUntil.IsZero() && a.FreshUntil.Before(e.FreshUntil) {
		e.FreshUntil = a.FreshUntil
	}
	if e.ExpiresAt.Before(e.FreshUntil) {
		e.FreshUntil = e.ExpiresAt
	}
	return e, now.Before(e.ExpiresAt)
}

// answer is the source's answer for a key as an entry: when it answered, and
// no bounds but the value's max age (see WithMaxAge). Set and SetMany write
// their values as if the source had answered them.
func (c *Cache[T]) answer(value T, notFound bool) Entry[T] {
	now := c.opts.now()
	e := Entry[T]{Value: value, NotFound: notFound, CachedAt: now}
	if !notFound && c.maxAge != nil {
		e.ExpiresAt = now.Add(c.maxAge(value))
		e.FreshUntil = e.ExpiresAt
		if !e.ExpiresAt.After(now) {
			e.ExpiresAt = now // zero means no bound; this one is already over
		}
	}
	return e
}

// put stores what the layer keeps for answer a under key, or deletes key
// there (see place).
func (l *Layer[T]) put(ctx context.Context, key string, a Entry[T], now time.Time) error {
	if e, ok := l.place(a, now); ok {
		return l.backend.Set(ctx, key, e)
	}
	return l.backend.Del(ctx, key)
}

// putMany is put for many answers, plus deleting dels, in one call each; keys
// in skip are left alone. It returns the keys that failed.
func (l *Layer[T]) putMany(ctx context.Context, answers map[string]Entry[T], dels []string, skip map[string]error, now time.Time) map[string]error {
	sets := map[string]Entry[T]{}
	for key, a := range answers {
		if _, ok := skip[key]; ok {
			continue
		}
		if e, ok := l.place(a, now); ok {
			sets[key] = e
		} else {
			dels = append(dels, key)
		}
	}
	failed := map[string]error{}
	if len(sets) > 0 {
		err := l.backend.SetMany(ctx, sets)
		for key := range sets {
			if kerr := errForKey(err, key); kerr != nil {
				failed[key] = kerr
			}
		}
	}
	if len(dels) > 0 {
		err := l.backend.DelMany(ctx, dels)
		for _, key := range dels {
			if kerr := errForKey(err, key); kerr != nil {
				failed[key] = kerr
			}
		}
	}
	return failed
}

type state int8

const (
	rotten state = iota // a miss: never served
	stale               // served, and refreshed in the background
	fresh
)

func (e *Entry[T]) state(now time.Time) state {
	switch {
	case !now.Before(e.ExpiresAt):
		return rotten
	case now.Before(e.FreshUntil):
		return fresh
	default:
		return stale
	}
}

// result is what a read of the entry returns.
func (e *Entry[T]) result() (T, error) {
	if e.NotFound {
		var zero T
		return zero, ErrNotFound
	}
	return e.Value, nil
}
