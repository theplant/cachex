package cachex

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ErrKeyNotFound indicates that the requested key was not found in the cache
type ErrKeyNotFound struct {
	Cached     bool  // whether this NotFound result was cached before
	CacheState State // the state of the cached NotFound entry (only meaningful when Cached=true)
}

// Error returns a string representation of the error
func (e *ErrKeyNotFound) Error() string {
	if !e.Cached {
		return "key not found"
	}

	switch e.CacheState {
	case StateFresh:
		return "key not found (cached, fresh)"
	case StateStale:
		return "key not found (cached, stale)"
	case StateRotten:
		return "key not found (cached, rotten)"
	default:
		return fmt.Sprintf("key not found (cached, state=%d)", e.CacheState)
	}
}

// IsErrKeyNotFound checks if the error is an ErrKeyNotFound
func IsErrKeyNotFound(err error) bool {
	if err == nil {
		return false
	}
	var e *ErrKeyNotFound
	return errors.As(err, &e)
}

// BatchError reports, per key, the keys that failed in a batch operation
// (GetMany). Keys that do not exist are not failures: they are just absent
// from the result map. errors.Is and errors.As see through to the per-key errors.
type BatchError struct {
	Errors map[string]error // failed key -> its error
}

// Error lists the failed keys in sorted order
func (e *BatchError) Error() string {
	keys := slices.Sorted(maps.Keys(e.Errors))
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = fmt.Sprintf("%s: %v", key, e.Errors[key])
	}
	return fmt.Sprintf("%d keys failed: %s", len(keys), strings.Join(parts, "; "))
}

// Unwrap returns the per-key errors, in sorted key order
func (e *BatchError) Unwrap() []error {
	keys := slices.Sorted(maps.Keys(e.Errors))
	errs := make([]error, len(keys))
	for i, key := range keys {
		errs[i] = e.Errors[key]
	}
	return errs
}
