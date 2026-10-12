package cachex

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"time"
)

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

// Broadcast carries invalidations between the instances of a Cache: the keys
// each write changed, so that the other instances drop them from their local
// layers (see WithBroadcast). Messages are opaque to it.
type Broadcast interface {
	// Publish sends msg to every instance listening; it may reach this one
	// too.
	Publish(ctx context.Context, msg []byte) error
	// Listen calls deliver with each message published, one at a time, until
	// ctx ends, and returns then. It calls ready each time it subscribes,
	// before delivering what is published from then on: what was published
	// while it was not subscribed is lost.
	Listen(ctx context.Context, ready func(), deliver func(msg []byte)) error
}

// WithBroadcast makes the Cache share its writes with the other instances
// over b: after a write, it publishes the keys it wrote, and an instance
// that receives them drops them from its local layers (those whose backend
// is a LocalBackend), through the write-order guard as a write of its own
// would. The shared layers were written by the writer. Each time b
// subscribes, the Cache clears its local layers, as a message may have been
// lost. A Cache with a broadcast needs a local layer. A failed publish is
// logged, not returned: the layers were written.
func WithBroadcast(b Broadcast) Option {
	return func(o *options) { o.broadcast = b }
}

// messageFormat is the first byte of a message; another value is not one.
const messageFormat = 1

// maxMessageKeys bounds the keys of one message; a larger write sends
// several.
const maxMessageKeys = 1000

// encodeMessage encodes keys written by the instance sender.
func encodeMessage(sender [16]byte, keys []string) []byte {
	n := 1 + len(sender)
	for _, key := range keys {
		n += binary.MaxVarintLen64 + len(key)
	}
	msg := make([]byte, 0, n)
	msg = append(msg, messageFormat)
	msg = append(msg, sender[:]...)
	for _, key := range keys {
		msg = binary.AppendUvarint(msg, uint64(len(key)))
		msg = append(msg, key...)
	}
	return msg
}

var errBadMessage = errors.New("cachex: not a broadcast message")

// decodeMessage decodes what encodeMessage encoded.
func decodeMessage(msg []byte) (sender [16]byte, keys []string, err error) {
	if len(msg) < 1+len(sender) || msg[0] != messageFormat {
		return sender, nil, errBadMessage
	}
	copy(sender[:], msg[1:])
	rest := msg[1+len(sender):]
	for len(rest) > 0 {
		n, w := binary.Uvarint(rest)
		if w <= 0 || n > uint64(len(rest)-w) {
			return sender, nil, errBadMessage
		}
		keys = append(keys, string(rest[w:w+int(n)]))
		rest = rest[w+int(n):]
	}
	return sender, keys, nil
}

// listen runs the broadcast's Listen until stop, again after a failure.
func (c *Cache[T]) listen(ctx context.Context) {
	for {
		err := c.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		c.opts.logger.ErrorContext(ctx, "cachex: broadcast listen failed", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// listenOnce runs the broadcast's Listen in a goroutine of its own, so that a
// runtime.Goexit under it (in a local layer, say) ends that call, not the
// listening: listen calls it again.
func (c *Cache[T]) listenOnce(ctx context.Context) (err error) {
	done := make(chan struct{})
	returned := false
	go func() {
		defer close(done)
		err = c.opts.broadcast.Listen(ctx, c.resubscribed, c.received)
		returned = true
	}()
	<-done
	if !returned {
		return errors.New("cachex: broadcast listener exited without returning (runtime.Goexit)")
	}
	return err
}

// publish sends the keys a write changed (or tried to) to the other
// instances.
func (c *Cache[T]) publish(ctx context.Context, keys []string) {
	if c.opts.broadcast == nil {
		return
	}
	keys = slices.DeleteFunc(slices.Clone(keys), func(key string) bool { return !supportedKey(key) })
	pctx, cancel := context.WithTimeout(sharedCtx(ctx), c.opts.fetchTimeout)
	defer cancel()
	for chunk := range slices.Chunk(keys, maxMessageKeys) {
		if err := c.opts.broadcast.Publish(pctx, encodeMessage(c.sender, chunk)); err != nil {
			c.opts.logger.WarnContext(pctx, "cachex: broadcast publish failed", "keys", len(keys), "error", err)
			return
		}
	}
}

// received drops the keys of another instance's message from the local
// layers, as a write would (see invalidateLocal).
func (c *Cache[T]) received(msg []byte) {
	sender, keys, err := decodeMessage(msg)
	if err != nil {
		c.opts.logger.Warn("cachex: dropped a broadcast message that does not decode", "error", err)
		return
	}
	if sender == c.sender || len(keys) == 0 {
		return
	}
	c.invalidateLocal(keys)
}

// resubscribed clears the local layers: a message may have been lost.
func (c *Cache[T]) resubscribed() { c.clearLocal() }
