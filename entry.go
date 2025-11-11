package cachex

import "time"

// Entry is a wrapper for a value with a cache timestamp
type Entry[T any] struct {
	Data     T         `json:"data"`
	CachedAt time.Time `json:"cachedAt"`
}

// EntryWithTTL is a convenience function to configure entry caching with TTL
// freshTTL: how long data stays fresh
// staleTTL: how long data stays stale (additional time after freshTTL)
// Entries in [0, freshTTL) are fresh, [freshTTL, freshTTL+staleTTL) are stale
func EntryWithTTL[T any](freshTTL, staleTTL time.Duration) ClientOption[*Entry[T]] {
	return WithStale(func(v *Entry[T]) State {
		age := NowFunc().Sub(v.CachedAt)
		if age < freshTTL {
			return StateFresh
		}
		if age < freshTTL+staleTTL {
			return StateStale
		}
		return StateTooStale
	})
}
