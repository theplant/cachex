package cachextest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
)

// Bus is an in-memory hub for cachex.Broadcast, for tests: each Broadcast it
// hands out stands for one instance. Messages reach the listening instances
// in the order they were published. Disconnect and Reconnect stand for a lost
// subscription: what is published in between is lost. A listener that falls
// 1024 messages behind holds Publish up until it catches up or goes away.
type Bus struct {
	// send lets one Publish or Reconnect run at a time, so messages keep
	// their order; it may wait on a slow listener, so mu, which a listener
	// takes to go away, is never held while sending
	send       sync.Mutex
	mu         sync.Mutex
	listeners  map[*listener]struct{}
	online     bool
	fail       error
	subscribed int
}

type listener struct {
	msgs  chan []byte
	ready chan struct{}
	gone  chan struct{} // closed when its Listen returns: nothing is sent to it then
}

// snapshot returns the listeners.
func (b *Bus) snapshot() []*listener {
	ls := make([]*listener, 0, len(b.listeners))
	for l := range b.listeners {
		ls = append(ls, l)
	}
	return ls
}

// NewBus returns a connected Bus.
func NewBus() *Bus { return &Bus{listeners: map[*listener]struct{}{}, online: true} }

// Broadcast returns a cachex.Broadcast on the bus, for one instance.
func (b *Bus) Broadcast() cachex.Broadcast { return busBroadcast{b} }

// Disconnect drops every subscription: messages published until Reconnect
// reach no one.
func (b *Bus) Disconnect() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.online = false
}

// Reconnect resubscribes every listener, each of which calls its ready.
func (b *Bus) Reconnect() {
	b.send.Lock()
	defer b.send.Unlock()
	b.mu.Lock()
	b.online = true
	ls := b.snapshot()
	b.mu.Unlock()
	for _, l := range ls {
		select {
		case l.ready <- struct{}{}:
		case <-l.gone:
		}
	}
}

// Subscribed is how many listeners have subscribed (and run their first
// ready) so far: wait for an instance's to know that a message published from
// then on reaches it and that its first clear is behind it.
func (b *Bus) Subscribed() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subscribed
}

// FailPublish makes Publish fail with err (nil: succeed again).
func (b *Bus) FailPublish(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = err
}

type busBroadcast struct{ b *Bus }

func (bb busBroadcast) Publish(_ context.Context, msg []byte) error {
	b := bb.b
	b.send.Lock()
	defer b.send.Unlock()
	b.mu.Lock()
	fail, online, ls := b.fail, b.online, b.snapshot()
	b.mu.Unlock()
	if fail != nil {
		return fail
	}
	if !online {
		return nil
	}
	for _, l := range ls {
		select {
		case l.msgs <- append([]byte(nil), msg...):
		case <-l.gone:
		}
	}
	return nil
}

func (bb busBroadcast) Listen(ctx context.Context, ready func(), deliver func(msg []byte)) error {
	b := bb.b
	l := &listener{msgs: make(chan []byte, 1024), ready: make(chan struct{}, 16), gone: make(chan struct{})}
	b.mu.Lock()
	b.listeners[l] = struct{}{}
	b.mu.Unlock()
	defer func() {
		close(l.gone)
		b.mu.Lock()
		delete(b.listeners, l)
		b.mu.Unlock()
	}()
	ready()
	b.mu.Lock()
	b.subscribed++
	b.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.ready:
			ready()
		case msg := <-l.msgs:
			deliver(msg)
		}
	}
}

// TestBroadcast is the contract test for a cachex.Broadcast. newPair returns
// two Broadcasts that reach each other, standing for two instances.
func TestBroadcast(t *testing.T, newPair func(t *testing.T) (a, b cachex.Broadcast)) {
	t.Helper()

	t.Run("a published message reaches the other instance", func(t *testing.T) {
		a, b := newPair(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := make(chan struct{}, 16)
		got := make(chan []byte, 16)
		done := make(chan error, 1)
		go func() {
			done <- b.Listen(ctx, func() { ready <- struct{}{} }, func(msg []byte) { got <- msg })
		}()
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			t.Fatal("Listen never called ready")
		}
		for _, msg := range [][]byte{[]byte("one"), {0, 1, 2, 255}} {
			require.NoError(t, a.Publish(context.Background(), msg))
			select {
			case m := <-got:
				assert.Equal(t, msg, m, "the bytes arrive as published")
			case <-time.After(10 * time.Second):
				t.Fatalf("%q never arrived", msg)
			}
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Listen did not return once its ctx ended")
		}
	})
}
