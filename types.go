package cachex

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrNotFound reports that a key does not exist. Sources return it from Get
// for a missing key; Cache.Get returns it for a key the source does not have,
// whether it was just asked or a cached not-found answered. Test with
// errors.Is.
var ErrNotFound = errors.New("cachex: not found")

// ErrUnsupportedKey reports a key that holds a "}" but no hash tag: a "{"
// followed, later, by a "}" with something between them, as Redis Cluster
// reads it. Such a key cannot share its Redis Cluster slot with the key of its
// lease, so the Cache rejects it whatever its layers, and a key that works now
// keeps working when a layer moves to Redis: Get, GetMany, Set and SetMany fail
// it, Del and DelMany do nothing (it was never stored). Test with errors.Is.
var ErrUnsupportedKey = errors.New("cachex: unsupported key: it holds a \"}\" but no hash tag")

// supportedKey reports whether key has a hash tag or no "}" (see
// ErrUnsupportedKey).
func supportedKey(key string) bool {
	if strings.IndexByte(key, '}') < 0 { // most keys: one scan
		return true
	}
	open := strings.IndexByte(key, '{')
	return open >= 0 && strings.IndexByte(key[open+1:], '}') > 0
}

func unsupportedKey(key string) error {
	return fmt.Errorf("cachex: key %q: %w", key, ErrUnsupportedKey)
}

// Entry is what a layer stores for one key: a value, or a record that the
// source does not have the key, and when it stops being fresh and usable.
// The Cache computes every field and never serves an entry past ExpiresAt;
// a backend stores it as given and may drop it from then on (rediscachex and
// ottercachex expire it natively).
type Entry[T any] struct {
	Value    T
	NotFound bool // the source does not have the key; Value is the zero value

	// CachedAt is when the source answered; an entry copied from a lower layer
	// keeps it.
	CachedAt time.Time
	// FreshUntil is when the entry turns stale: still served (and refreshed in
	// the background) until ExpiresAt, if the layer has a stale TTL.
	FreshUntil time.Time
	// ExpiresAt is when the entry is rotten: never served again. A backend may
	// drop it from then on.
	ExpiresAt time.Time
}

// Backend stores the entries of one layer. A missing key is not an error:
// Get reports it with false, GetMany leaves it out of the map. The Many
// methods are best effort: a failure of some keys is reported as a *BatchError
// listing them (returned as is, not wrapped); any other error means the whole
// call failed. A store with single-key operations only can implement
// SingleBackend and be wrapped with Batched.
type Backend[T any] interface {
	Get(ctx context.Context, key string) (Entry[T], bool, error)
	GetMany(ctx context.Context, keys []string) (map[string]Entry[T], error)
	Set(ctx context.Context, key string, entry Entry[T]) error
	SetMany(ctx context.Context, entries map[string]Entry[T]) error
	Del(ctx context.Context, key string) error
	DelMany(ctx context.Context, keys []string) error
}

// SliceGetter is an optional Backend method for the first layer: GetInto
// reads keys into entries and found (both as long as keys), entries[i] and
// found[i] being what Get returns for keys[i], with errors as GetMany's. A
// GetMany of the Cache uses it instead of the backend's GetMany: that map
// allocates once per entry when Entry[T] is over 128 bytes (a value type of
// over 48 bytes, say), as Go maps hold larger values out of line. A wrapper
// that embeds a backend having GetInto and overrides GetMany must override
// GetInto too (or embed the Backend interface), or reads bypass its GetMany.
type SliceGetter[T any] interface {
	GetInto(ctx context.Context, keys []string, entries []Entry[T], found []bool) error
}

// SingleBackend is a Backend without the Many methods; see Batched.
type SingleBackend[T any] interface {
	Get(ctx context.Context, key string) (Entry[T], bool, error)
	Set(ctx context.Context, key string, entry Entry[T]) error
	Del(ctx context.Context, key string) error
}

// Source is where the data lives, asked when no layer has a usable entry. Get
// returns ErrNotFound (possibly wrapped) for a key it does not have.
type Source[T any] interface {
	Get(ctx context.Context, key string) (T, error)
}

// SourceFunc adapts a function to a Source.
type SourceFunc[T any] func(ctx context.Context, key string) (T, error)

// Get calls f.
func (f SourceFunc[T]) Get(ctx context.Context, key string) (T, error) { return f(ctx, key) }

// BatchSource is a Source that can answer many keys in one call; GetMany uses
// it when the source implements it. A key the source does not have is absent
// from the map (not an error). A *BatchError returned as is fails only the
// keys it lists; any other error fails every key. An error that wraps
// ErrNotFound (for a key, or for the call) reads its keys as not found but
// caches nothing, so each read asks again: leave missing keys out instead.
type BatchSource[T any] interface {
	GetMany(ctx context.Context, keys []string) (map[string]T, error)
}

// BatchError lists the keys a batch operation failed for. Keys that do not
// exist are never in it.
type BatchError struct {
	Errors map[string]error
}

func (e *BatchError) Error() string {
	keys := make([]string, 0, len(e.Errors))
	for key := range e.Errors {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	const shown = 3
	var b strings.Builder
	fmt.Fprintf(&b, "cachex: %d keys failed", len(keys))
	for i, key := range keys {
		if i == shown {
			fmt.Fprintf(&b, "; and %d more", len(keys)-shown)
			break
		}
		fmt.Fprintf(&b, "; %s: %v", key, e.Errors[key])
	}
	return b.String()
}

// NewBatchError returns a *BatchError listing errs, or nil if errs is empty:
// what a backend's Many method returns after trying every key.
func NewBatchError(errs map[string]error) error { return batchError(errs) }

// batchError returns a *BatchError for errs, or nil if errs is empty.
func batchError(errs map[string]error) error {
	if len(errs) == 0 {
		return nil
	}
	return &BatchError{Errors: errs}
}

// errForKey returns the error a batch call reported for key: the key's own
// entry if err is a *BatchError, otherwise err itself (the whole call failed).
// Only an unwrapped *BatchError is partial: a wrapped or joined one may sit
// next to an error that failed the whole call. A nil *BatchError, returned
// by mistake as an error that is not nil, fails no key.
func errForKey(err error, key string) error {
	if batchErr, ok := err.(*BatchError); ok { //nolint:errorlint // see above
		if batchErr == nil {
			return nil
		}
		return batchErr.Errors[key]
	}
	return err
}

// Batched turns a SingleBackend into a Backend whose Many methods go key by
// key, best effort: the keys that fail are reported in a *BatchError. It
// forwards b's LocalBackend and Leasing, if b has them.
func Batched[T any](b SingleBackend[T]) Backend[T] {
	if _, local := b.(LocalBackend); local {
		return batchedLocal[T]{batched[T]{b}}
	}
	return batched[T]{b}
}

type batched[T any] struct{ SingleBackend[T] }

func (b batched[T]) Leaser() Leaser[T] {
	if l, ok := b.SingleBackend.(Leasing[T]); ok {
		return l.Leaser()
	}
	return nil
}

type batchedLocal[T any] struct{ batched[T] }

func (b batchedLocal[T]) Clear(ctx context.Context) error {
	return b.SingleBackend.(LocalBackend).Clear(ctx)
}

// GetInto is GetMany into slices (see SliceGetter).
func (b batched[T]) GetInto(ctx context.Context, keys []string, entries []Entry[T], found []bool) error {
	errs := map[string]error{}
	for i, key := range keys {
		e, ok, err := b.Get(ctx, key)
		switch {
		case err != nil:
			errs[key] = err
		case ok:
			entries[i], found[i] = e, true
		}
	}
	return batchError(errs)
}

func (b batched[T]) GetMany(ctx context.Context, keys []string) (map[string]Entry[T], error) {
	out := make(map[string]Entry[T], len(keys))
	errs := map[string]error{}
	for _, key := range keys {
		e, ok, err := b.Get(ctx, key)
		switch {
		case err != nil:
			errs[key] = err
		case ok:
			out[key] = e
		}
	}
	return out, batchError(errs)
}

func (b batched[T]) SetMany(ctx context.Context, entries map[string]Entry[T]) error {
	errs := map[string]error{}
	for key, e := range entries {
		if err := b.Set(ctx, key, e); err != nil {
			errs[key] = err
		}
	}
	return batchError(errs)
}

func (b batched[T]) DelMany(ctx context.Context, keys []string) error {
	errs := map[string]error{}
	for _, key := range keys {
		if err := b.Del(ctx, key); err != nil {
			errs[key] = err
		}
	}
	return batchError(errs)
}
