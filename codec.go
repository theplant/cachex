package cachex

import (
	"encoding"
	"encoding/json"
	"fmt"
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
