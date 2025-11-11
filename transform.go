package cachex

import (
	"context"
	"encoding/json"

	"github.com/pkg/errors"
)

// transformCache wraps a Cache[A] and provides type transformation to Cache[B]
type transformCache[A, B any] struct {
	cache  Cache[A]
	encode func(B) (A, error)
	decode func(A) (B, error)
}

// Transform creates a new TransformCache with custom encode/decode functions
func Transform[A, B any](
	cache Cache[A],
	encode func(B) (A, error),
	decode func(A) (B, error),
) Cache[B] {
	return &transformCache[A, B]{
		cache:  cache,
		encode: encode,
		decode: decode,
	}
}

// Set encodes the value and stores it in the underlying cache
func (t *transformCache[A, B]) Set(ctx context.Context, key string, value B) error {
	encoded, err := t.encode(value)
	if err != nil {
		return errors.Wrap(err, "failed to encode value")
	}
	return t.cache.Set(ctx, key, encoded)
}

// Get retrieves the value from the underlying cache and decodes it
func (t *transformCache[A, B]) Get(ctx context.Context, key string) (B, error) {
	var zero B
	encoded, err := t.cache.Get(ctx, key)
	if err != nil {
		return zero, err
	}
	decoded, err := t.decode(encoded)
	if err != nil {
		return zero, errors.Wrap(err, "failed to decode value")
	}
	return decoded, nil
}

// Del removes the value from the underlying cache
func (t *transformCache[A, B]) Del(ctx context.Context, key string) error {
	return t.cache.Del(ctx, key)
}

// JSONTransform creates a TransformCache that uses JSON encoding/decoding
// to convert between Cache[[]byte] and Cache[T]
func JSONTransform[T any](cache Cache[[]byte]) Cache[T] {
	return Transform(
		cache,
		func(value T) ([]byte, error) {
			return json.Marshal(value)
		},
		func(data []byte) (T, error) {
			var value T
			err := json.Unmarshal(data, &value)
			return value, err
		},
	)
}

// StringJSONTransform creates a TransformCache that uses JSON encoding/decoding
// to convert between Cache[string] and Cache[T]
func StringJSONTransform[T any](cache Cache[string]) Cache[T] {
	return Transform(
		cache,
		func(value T) (string, error) {
			data, err := json.Marshal(value)
			if err != nil {
				return "", err
			}
			return string(data), nil
		},
		func(data string) (T, error) {
			var value T
			err := json.Unmarshal([]byte(data), &value)
			return value, err
		},
	)
}
