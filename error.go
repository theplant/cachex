package cachex

import (
	"errors"
	"fmt"
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
