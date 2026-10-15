package cachex

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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

// Encoded turns a ByteBackend into a Backend that stores each entry as bytes:
// its times, whether it is a not-found, and its value as codec (nil:
// DefaultCodec) marshals it. Data that does not decode is a miss, not a
// failure: it was written by something else, or before the value's type
// changed, so Encoded logs a WARN to logger (nil: slog.Default()), deletes the
// key, and reports it absent. An entry already past its ExpiresAt is not
// written: its key is deleted instead. That is judged by the wall clock
// (time.Now), as backends expire entries by it, whatever clock the Cache uses.
// It forwards ByteLeasing (as Leasing) and ByteLocalBackend (as LocalBackend).
func Encoded[T any](b ByteBackend, codec Codec[T], logger *slog.Logger) Backend[T] {
	if codec == nil {
		codec = DefaultCodec[T]()
	}
	if logger == nil {
		logger = slog.Default()
	}
	e := &encoded[T]{b: b, codec: codec, logger: logger}
	if _, local := b.(ByteLocalBackend); local {
		return encodedLocal[T]{e}
	}
	return e
}

type encoded[T any] struct {
	b      ByteBackend
	codec  Codec[T]
	logger *slog.Logger
}

func (e *encoded[T]) decode(ctx context.Context, key string, data []byte) (Entry[T], bool) {
	entry, err := decodeEntry(e.codec, data)
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
	e.decodeMany(ctx, keys, m, func(i int, entry Entry[T]) { out[keys[i]] = entry })
	return out, err
}

// GetInto is GetMany into slices (see SliceGetter).
func (e *encoded[T]) GetInto(ctx context.Context, keys []string, entries []Entry[T], found []bool) error {
	m, err := e.b.GetMany(ctx, keys)
	if _, partial := err.(*BatchError); err != nil && !partial { //nolint:errorlint // only an unwrapped one is partial
		return err
	}
	e.decodeMany(ctx, keys, m, func(i int, entry Entry[T]) { entries[i], found[i] = entry, true })
	return err
}

// decodeMany decodes what m holds of keys, passing each entry to put with
// its key's index; data that does not decode is deleted (see Encoded).
func (e *encoded[T]) decodeMany(ctx context.Context, keys []string, m map[string][]byte, put func(i int, entry Entry[T])) {
	var bad []string
	for i, key := range keys {
		data, ok := m[key]
		if !ok {
			continue
		}
		if entry, ok := e.decode(ctx, key, data); ok {
			put(i, entry)
		} else {
			bad = append(bad, key)
		}
	}
	if len(bad) > 0 {
		_ = e.b.DelMany(ctx, bad)
	}
}

func (e *encoded[T]) Set(ctx context.Context, key string, entry Entry[T]) error {
	if !entry.ExpiresAt.After(time.Now()) {
		return e.b.Del(ctx, key)
	}
	data, err := encodeEntry(e.codec, entry)
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
		data, err := encodeEntry(e.codec, entry)
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

// entryFormat is the first byte of an encoded entry; another value means the
// data was written by something else, or by an incompatible version.
const entryFormat = 1

const entryHeader = 2 + 3*8 // format, flags, three times

// encodeEntry encodes an entry for a ByteBackend: its times and
// whether it is a not-found, then its value as codec marshals it.
func encodeEntry[T any](codec Codec[T], e Entry[T]) ([]byte, error) {
	var value []byte
	var flags byte
	if e.NotFound {
		flags = 1
	} else {
		var err error
		if value, err = codec.Marshal(e.Value); err != nil {
			return nil, fmt.Errorf("cachex: encode entry: %w", err)
		}
	}
	data := make([]byte, entryHeader, entryHeader+len(value))
	data[0], data[1] = entryFormat, flags
	for i, t := range []time.Time{e.CachedAt, e.FreshUntil, e.ExpiresAt} {
		var n int64
		if !t.IsZero() {
			n = t.UnixNano()
		}
		binary.BigEndian.PutUint64(data[2+8*i:], uint64(n))
	}
	return append(data, value...), nil
}

var errBadEntry = errors.New("cachex: not an encoded entry")

// decodeEntry decodes what encodeEntry encoded. Data that does not decode was
// written by something else, or before the value's type changed: Encoded
// treats it as a miss.
func decodeEntry[T any](codec Codec[T], data []byte) (Entry[T], error) {
	if len(data) < entryHeader || data[0] != entryFormat || data[1]&^1 != 0 {
		return Entry[T]{}, errBadEntry
	}
	var e Entry[T]
	e.NotFound = data[1] == 1
	for i, t := range []*time.Time{&e.CachedAt, &e.FreshUntil, &e.ExpiresAt} {
		if n := int64(binary.BigEndian.Uint64(data[2+8*i:])); n != 0 {
			*t = time.Unix(0, n).UTC()
		}
	}
	if e.NotFound {
		if len(data) != entryHeader {
			return Entry[T]{}, errBadEntry
		}
		return e, nil
	}
	value, err := codec.Unmarshal(data[entryHeader:])
	if err != nil {
		return Entry[T]{}, fmt.Errorf("cachex: decode entry: %w", err)
	}
	e.Value = value
	return e, nil
}
