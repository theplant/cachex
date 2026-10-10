package cachex

import (
	"context"
	"log/slog"
	"time"
)

// ByteBackend stores bytes by key, for a backend that stores entries encoded:
// wrap it with Encoded to get a Backend. Like Backend, a missing key is not an
// error (Get reports it with false, GetMany leaves it out of the map), and
// the Many methods are best effort: a failure of some keys is a *BatchError
// listing them, returned as is; any other error means the whole call failed.
// The single-key methods return plain errors.
type ByteBackend interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	GetMany(ctx context.Context, keys []string) (map[string][]byte, error)
	// Set stores data until expiresAt, which Encoded passes only in the
	// future; the backend may drop the key from then on.
	Set(ctx context.Context, key string, data []byte, expiresAt time.Time) error
	SetMany(ctx context.Context, entries map[string]ByteEntry) error
	Del(ctx context.Context, key string) error
	DelMany(ctx context.Context, keys []string) error
}

// ByteEntry is an encoded entry for ByteBackend.SetMany, and when it expires.
type ByteEntry struct {
	Data      []byte
	ExpiresAt time.Time
}

// Encoded turns a ByteBackend into a Backend that stores each entry as
// EncodeEntry encodes it with codec (nil: DefaultCodec). Data that does not
// decode is a miss, not a failure: it was written by something else, or before
// the value's type changed, so Encoded logs a WARN to logger (nil:
// slog.Default()), deletes the key, and reports it absent. An entry already
// past its ExpiresAt is not written: its key is deleted instead. That is
// judged by the wall clock (time.Now), as backends expire entries by it,
// whatever clock the Cache uses.
func Encoded[T any](b ByteBackend, codec Codec[T], logger *slog.Logger) Backend[T] {
	if codec == nil {
		codec = DefaultCodec[T]()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &encoded[T]{b: b, codec: codec, logger: logger}
}

type encoded[T any] struct {
	b      ByteBackend
	codec  Codec[T]
	logger *slog.Logger
}

func (e *encoded[T]) decode(ctx context.Context, key string, data []byte) (Entry[T], bool) {
	entry, err := DecodeEntry(e.codec, data)
	if err != nil {
		e.logger.WarnContext(ctx, "cachex: dropped an entry that does not decode", "key", key, "error", err)
		return Entry[T]{}, false
	}
	return entry, true
}

func (e *encoded[T]) Get(ctx context.Context, key string) (Entry[T], bool, error) {
	data, ok, err := e.b.Get(ctx, key)
	if err != nil || !ok {
		return Entry[T]{}, false, err
	}
	entry, ok := e.decode(ctx, key, data)
	if !ok {
		_ = e.b.Del(ctx, key)
	}
	return entry, ok, nil
}

func (e *encoded[T]) GetMany(ctx context.Context, keys []string) (map[string]Entry[T], error) {
	m, err := e.b.GetMany(ctx, keys)
	if _, partial := err.(*BatchError); err != nil && !partial { //nolint:errorlint // only an unwrapped one is partial
		return nil, err
	}
	out := make(map[string]Entry[T], len(m))
	var bad []string
	for key, data := range m {
		if entry, ok := e.decode(ctx, key, data); ok {
			out[key] = entry
		} else {
			bad = append(bad, key)
		}
	}
	if len(bad) > 0 {
		_ = e.b.DelMany(ctx, bad)
	}
	return out, err
}

func (e *encoded[T]) Set(ctx context.Context, key string, entry Entry[T]) error {
	if !entry.ExpiresAt.After(time.Now()) {
		return e.b.Del(ctx, key)
	}
	data, err := EncodeEntry(e.codec, entry)
	if err != nil {
		return err
	}
	return e.b.Set(ctx, key, data, entry.ExpiresAt)
}

func (e *encoded[T]) SetMany(ctx context.Context, entries map[string]Entry[T]) error {
	errs := map[string]error{}
	now := time.Now()
	live := make(map[string]ByteEntry, len(entries))
	var expired []string
	for key, entry := range entries {
		if !entry.ExpiresAt.After(now) {
			expired = append(expired, key)
			continue
		}
		data, err := EncodeEntry(e.codec, entry)
		if err != nil {
			errs[key] = err
			continue
		}
		live[key] = ByteEntry{Data: data, ExpiresAt: entry.ExpiresAt}
	}
	if len(live) > 0 {
		err := e.b.SetMany(ctx, live)
		for key := range live {
			if kerr := errForKey(err, key); kerr != nil {
				errs[key] = kerr
			}
		}
	}
	if len(expired) > 0 {
		err := e.b.DelMany(ctx, expired)
		for _, key := range expired {
			if kerr := errForKey(err, key); kerr != nil {
				errs[key] = kerr
			}
		}
	}
	return batchError(errs)
}

func (e *encoded[T]) Del(ctx context.Context, key string) error { return e.b.Del(ctx, key) }

func (e *encoded[T]) DelMany(ctx context.Context, keys []string) error {
	return e.b.DelMany(ctx, keys)
}
