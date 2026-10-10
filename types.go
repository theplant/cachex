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

// Entry is what a layer stores for one key: a value, or a record that the
// source does not have the key, and when it stops being fresh and usable.
// Backends store it as given and treat ExpiresAt as the entry's native
// expiry; the Cache computes every field.
type Entry[T any] struct {
	Value    T
	NotFound bool // the source does not have the key; Value is the zero value

	// CachedAt is when the source answered; an entry copied from a lower layer
	// keeps it, so an entry's age is not reset layer by layer.
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
// keys it lists; any other error fails every key.
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
// next to an error that failed the whole call.
func errForKey(err error, key string) error {
	if batchErr, ok := err.(*BatchError); ok { //nolint:errorlint // see above
		return batchErr.Errors[key]
	}
	return err
}
