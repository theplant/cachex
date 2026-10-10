package cachex

import (
	"math/rand/v2"
	"time"
)

// DefaultJitter is the jitter of a layer that does not set one (see Jitter).
const DefaultJitter = 0.1

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
// lived than the entry it came from. Required, with a positive fresh.
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
	case cfg.jitter < 0 || cfg.jitter >= 1:
		panic("cachex: NewLayer: jitter must be in [0, 1)")
	}
	return Layer[T]{backend: backend, cfg: cfg}
}

// entry is what the layer stores, from now on, for an answer the source gave
// at cachedAt (a value, or notFound): the layer's TTLs count from now, and the
// entry is no fresher than freshCap and no longer usable than expiresCap (zero:
// no cap), the bounds of the entry it was copied from. So an upper layer keeps
// a copy for its own TTL, but never past the entry below. It returns false if
// the layer does not keep such an answer.
func (l *Layer[T]) entry(value T, notFound bool, cachedAt, now, freshCap, expiresCap time.Time) (Entry[T], bool) {
	fresh, stale := l.cfg.fresh, l.cfg.stale
	if notFound {
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
		NotFound:   notFound,
		CachedAt:   cachedAt,
		FreshUntil: now.Add(fresh),
		ExpiresAt:  now.Add(fresh + stale),
	}
	if !expiresCap.IsZero() && expiresCap.Before(e.ExpiresAt) {
		e.ExpiresAt = expiresCap
	}
	if !freshCap.IsZero() && freshCap.Before(e.FreshUntil) {
		e.FreshUntil = freshCap
	}
	if e.ExpiresAt.Before(e.FreshUntil) {
		e.FreshUntil = e.ExpiresAt
	}
	return e, true
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
