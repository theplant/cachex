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
// subscription: what is published in between is lost.
type Bus struct {
	mu         sync.Mutex
	listeners  map[*listener]struct{}
	online     bool
	fail       error
	subscribed int
}

type listener struct {
	msgs  chan []byte
	ready chan struct{}
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
	b.mu.Lock()
	defer b.mu.Unlock()
	b.online = true
	for l := range b.listeners {
		l.ready <- struct{}{}
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
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	if !b.online {
		return nil
	}
	for l := range b.listeners {
		l.msgs <- append([]byte(nil), msg...)
	}
	return nil
}

func (bb busBroadcast) Listen(ctx context.Context, ready func(), deliver func(msg []byte)) error {
	b := bb.b
	l := &listener{msgs: make(chan []byte, 1024), ready: make(chan struct{}, 16)}
	b.mu.Lock()
	b.listeners[l] = struct{}{}
	b.mu.Unlock()
	defer func() {
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
