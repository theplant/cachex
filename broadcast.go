package cachex

import "context"

// LocalBackend is a Backend whose entries live in this process only, such as
// one in memory: a write on another instance never reaches it through a
// shared layer. Its layers are the ones a broadcast invalidates (see
// WithBroadcast), and Clear drops all of their entries when a broadcast may
// have been missed. A backend shared by instances must not implement it.
type LocalBackend interface {
	Clear(ctx context.Context) error
}

// ByteLocalBackend is LocalBackend for a ByteBackend; Encoded forwards it.
type ByteLocalBackend interface {
	Clear(ctx context.Context) error
}

// encodedLocal is an encoded ByteLocalBackend: a LocalBackend.
type encodedLocal[T any] struct{ *encoded[T] }

func (e encodedLocal[T]) Clear(ctx context.Context) error {
	return e.b.(ByteLocalBackend).Clear(ctx)
}
