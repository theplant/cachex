package cachex_test

import (
	"bytes"
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// instance is one process: its own memory layer over a layer every instance
// shares, all on one source and one bus.
type instance struct {
	c     *cachex.Cache[string]
	local *cachextest.Map[string]
}

func newInstance(t *testing.T, src cachex.Source[string], shared cachex.Backend[string], bus *cachextest.Bus, opts ...cachex.Option) instance {
	t.Helper()
	local := cachextest.NewMap[string]()
	subscribed := bus.Subscribed()
	c := cachex.New(src, []cachex.Layer[string]{
		cachex.NewLayer(local, cachex.TTL(time.Hour, 0)),
		cachex.NewLayer(shared, cachex.TTL(time.Hour, 0)),
	}, append([]cachex.Option{cachex.WithBroadcast(bus.Broadcast())}, opts...)...)
	t.Cleanup(func() { _ = c.Close() })
	require.Eventually(t, func() bool { return bus.Subscribed() > subscribed }, 5*time.Second, time.Millisecond,
		"listening, its first clear done")
	return instance{c: c, local: local}
}

func eventuallyGone(t *testing.T, m *cachextest.Map[string], key string, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { _, ok := stored(t, m, key); return !ok }, 5*time.Second, time.Millisecond, msg)
}

func TestAWriteReachesTheOtherInstancesLocalLayers(t *testing.T) {
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1", "other": "o"})
	shared := cachextest.NewLeasingMap[string]()
	bus := cachextest.NewBus()
	a, b := newInstance(t, src, shared, bus), newInstance(t, src, shared, bus)

	for _, in := range []instance{a, b} {
		v, err := in.c.Get(ctx, "k")
		require.NoError(t, err)
		require.Equal(t, "v1", v)
		_, err = in.c.Get(ctx, "other")
		require.NoError(t, err)
		cachex.Settle(in.c)
	}

	src.set("k", "v2")
	require.NoError(t, a.c.Set(ctx, "k", "v2"))
	eventuallyGone(t, b.local, "k", "b dropped the key from its memory layer")
	v, err := b.c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v, "and reads the write from the shared layer")
	e, ok := stored(t, shared, "k")
	require.True(t, ok, "the shared layer is left alone")
	assert.Equal(t, "v2", e.Value)

	// a's own message reaches a too, in order before this one: a ignores it
	require.NoError(t, b.c.Del(ctx, "other"))
	eventuallyGone(t, a.local, "other", "a drops what b wrote")
	e, ok = stored(t, a.local, "k")
	require.True(t, ok, "a kept its own write")
	assert.Equal(t, "v2", e.Value)
}

func TestAResubscriptionClearsTheLocalLayers(t *testing.T) {
	// what was published while an instance was not subscribed is lost: it
	// drops everything local instead
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	shared := cachextest.NewLeasingMap[string]()
	bus := cachextest.NewBus()
	a, b := newInstance(t, src, shared, bus), newInstance(t, src, shared, bus)
	_, err := b.c.Get(ctx, "k")
	require.NoError(t, err)
	cachex.Settle(b.c)

	bus.Disconnect()
	src.set("k", "v2")
	require.NoError(t, a.c.Set(ctx, "k", "v2")) // b misses this
	bus.Reconnect()
	eventuallyGone(t, b.local, "k", "b cleared its memory layer")
	v, err := b.c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v)
}

func TestABroadcastInvalidatesThroughTheWriteOrderGuard(t *testing.T) {
	// b is fetching v1 when a's write arrives: b's fetch must not backfill v1
	// into b's memory layer afterwards
	ctx := context.Background()
	var data atomic.Value
	data.Store("v1")
	asked, gate := make(chan struct{}, 1), make(chan struct{})
	src := cachex.SourceFunc[string](func(_ context.Context, key string) (string, error) {
		if key != "k" {
			return "", cachex.ErrNotFound
		}
		v := data.Load().(string) // read before the write, returned after it
		select {
		case asked <- struct{}{}:
		default:
		}
		<-gate
		return v, nil
	})
	shared := cachextest.NewLeasingMap[string]()
	bus := cachextest.NewBus()
	a, b := newInstance(t, src, shared, bus), newInstance(t, src, shared, bus)

	require.NoError(t, b.c.Set(ctx, "x", "x")) // in b's memory layer
	got := make(chan string)
	go func() {
		v, _ := b.c.Get(ctx, "k")
		got <- v
	}()
	<-asked // b's source request read v1; the gate holds it
	data.Store("v2")
	require.NoError(t, a.c.Set(ctx, "k", "v2"))
	// the bus delivers in order: once a's later message about x has reached
	// b, b has handled the one about k
	require.NoError(t, a.c.Del(ctx, "x"))
	eventuallyGone(t, b.local, "x", "b handled a's messages")
	close(gate)
	<-got
	require.NoError(t, b.c.Close())
	if e, ok := stored(t, b.local, "k"); ok {
		assert.Equal(t, "v2", e.Value, "b's fetch from before the write did not land")
	}
}

func TestAFailedPublishDoesNotFailTheWrite(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	bus := cachextest.NewBus()
	a := newInstance(t, newSource(nil), cachextest.NewLeasingMap[string](), bus,
		cachex.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	bus.FailPublish(errBoom)
	require.NoError(t, a.c.Set(ctx, "k", "v"), "the layers were written")
	assert.Contains(t, logs.String(), "broadcast")
}

func TestABroadcastNeedsALocalLayer(t *testing.T) {
	assert.Panics(t, func() {
		cachex.New[string](newSource(nil), []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewLeasingMap[string](), cachex.TTL(time.Hour, 0)),
		}, cachex.WithBroadcast(cachextest.NewBus().Broadcast()))
	})
}

func TestAMessageThatDoesNotDecodeIsIgnored(t *testing.T) {
	ctx := context.Background()
	bus := cachextest.NewBus()
	a := newInstance(t, newSource(map[string]string{"k": "v"}), cachextest.NewLeasingMap[string](), bus)
	_, err := a.c.Get(ctx, "k")
	require.NoError(t, err)
	cachex.Settle(a.c)
	require.NoError(t, bus.Broadcast().Publish(ctx, []byte("not a message")))
	_, ok := stored(t, a.local, "k")
	assert.True(t, ok)
	v, err := a.c.Get(ctx, "k")
	require.NoError(t, err, "the listener goes on")
	assert.Equal(t, "v", v)
}
