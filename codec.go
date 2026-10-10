package cachex

import (
	"context"
	"encoding"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Codec turns values into bytes and back, for backends that store bytes.
type Codec[T any] interface {
	Marshal(value T) ([]byte, error)
	Unmarshal(data []byte) (T, error)
}

// JSONCodec encodes values as JSON.
func JSONCodec[T any]() Codec[T] { return jsonCodec[T]{} }

type jsonCodec[T any] struct{}

func (jsonCodec[T]) Marshal(value T) ([]byte, error) { return json.Marshal(value) }

func (jsonCodec[T]) Unmarshal(data []byte) (T, error) {
	var value T
	err := json.Unmarshal(data, &value)
	return value, err
}

// DefaultCodec stores []byte and string values as they are, values whose T
// implements encoding.BinaryMarshaler (and *T BinaryUnmarshaler) as they
// marshal themselves, and anything else as JSON.
func DefaultCodec[T any]() Codec[T] {
	var zero T
	switch any(zero).(type) {
	case []byte, string:
		return rawCodec[T]{}
	}
	_, m := any(zero).(encoding.BinaryMarshaler)
	_, u := any(&zero).(encoding.BinaryUnmarshaler)
	if m && u {
		return binaryCodec[T]{}
	}
	return jsonCodec[T]{}
}

type rawCodec[T any] struct{}

func (rawCodec[T]) Marshal(value T) ([]byte, error) {
	switch v := any(value).(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	}
	return nil, fmt.Errorf("cachex: raw codec cannot marshal %T", value)
}

func (rawCodec[T]) Unmarshal(data []byte) (T, error) {
	var value T
	switch p := any(&value).(type) {
	case *[]byte:
		*p = data
	case *string:
		*p = string(data)
	}
	return value, nil
}

type binaryCodec[T any] struct{}

func (binaryCodec[T]) Marshal(value T) ([]byte, error) {
	return any(value).(encoding.BinaryMarshaler).MarshalBinary()
}

func (binaryCodec[T]) Unmarshal(data []byte) (T, error) {
	var value T
	err := any(&value).(encoding.BinaryUnmarshaler).UnmarshalBinary(data)
	return value, err
}

// entryFormat is the first byte of an encoded entry; another value means the
// data was written by something else, or by an incompatible version.
const entryFormat = 1

const entryHeader = 2 + 3*8 // format, flags, three times

// EncodeEntry encodes an entry for a backend that stores bytes: its times and
// whether it is a not-found, then its value as codec marshals it.
func EncodeEntry[T any](codec Codec[T], e Entry[T]) ([]byte, error) {
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

// DecodeEntry decodes what EncodeEntry encoded. Backends should treat data
// that does not decode as a miss (and may drop it): it was written by
// something else, or before the value's type changed.
func DecodeEntry[T any](codec Codec[T], data []byte) (Entry[T], error) {
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

// Batched turns a SingleBackend into a Backend whose Many methods go key by
// key, best effort: the keys that fail are reported in a *BatchError.
func Batched[T any](b SingleBackend[T]) Backend[T] { return batched[T]{b} }

type batched[T any] struct{ SingleBackend[T] }

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
