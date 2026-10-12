package cachex_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
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

func TestABroadcastInvalidationThatGaveUpLeavesTheSharedLayersAlone(t *testing.T) {
	// a's write of k holds k's stripe while b's message about k arrives; a's
	// invalidation gives up waiting, and drops k once the stripe is free: from
	// the local layers only, never from the shared one a's write just wrote
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	shared := cachextest.NewLeasingMap[string]() // not a LocalBackend
	bl := newBlocker("k")
	var logs syncBuffer
	bus := cachextest.NewBus()
	a := newInstance(t, src, bl.wrap(shared), bus, cachex.WithFetchTimeout(20*time.Millisecond),
		cachex.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	b := newInstance(t, src, shared, bus)

	done := make(chan error)
	go func() { done <- a.c.Set(ctx, "k", "a") }()
	<-bl.entered
	require.NoError(t, b.c.Del(ctx, "k"))
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), "waited too long") }, 5*time.Second, time.Millisecond)
	close(bl.release)
	require.NoError(t, <-done)
	cachex.Settle(a.c) // the late invalidation ran
	e, ok := stored(t, shared, "k")
	require.True(t, ok, "a's write, the last one, stands in the shared layer")
	assert.Equal(t, "a", e.Value)
	_, ok = stored(t, a.local, "k")
	assert.False(t, ok, "a dropped k from its memory layer")
}

// syncBuffer is a bytes.Buffer safe for a logger and a test to share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// localHooked is a hooked Map that is still a LocalBackend.
type localHooked struct {
	cachex.Backend[string]
	m *cachextest.Map[string]
}

func (l localHooked) Clear(ctx context.Context) error { return l.m.Clear(ctx) }

func TestAWriteThatGaveUpPublishesAgainAfterItsDrop(t *testing.T) {
	// a's write gives up while a's backfill of the old value holds the stripe,
	// and publishes; b copies the old value from the shared layer before a's
	// late drop: the drop is published too, so b does not keep it
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v0"})
	shared := cachextest.NewLeasingMap[string]()
	bus := cachextest.NewBus()

	// a: its memory layer holds the backfill of k (after the shared layer
	// got it), so the backfill keeps k's stripe read-locked
	aMap := cachextest.NewMap[string]()
	bl := newBlocker("k")
	sub := bus.Subscribed()
	a := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer[string](localHooked{bl.wrap(aMap), aMap}, cachex.TTL(time.Hour, 0)),
		cachex.NewLayer[string](shared, cachex.TTL(time.Hour, 0)),
	}, cachex.WithBroadcast(bus.Broadcast()))
	t.Cleanup(func() { _ = a.Close() })
	require.Eventually(t, func() bool { return bus.Subscribed() > sub }, 5*time.Second, time.Millisecond)
	b := newInstance(t, src, shared, bus)

	v, err := a.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v0", v)
	<-bl.entered // backfill: shared has v0, a's memory layer write is held

	// b holds something for k locally, so we can see it handle the message
	require.NoError(t, b.local.Set(ctx, "k", cachex.Entry[string]{Value: "marker", CachedAt: epoch, FreshUntil: time.Now().Add(time.Hour), ExpiresAt: time.Now().Add(time.Hour)}))

	src.set("k", "v2")
	wctx, cancel := context.WithCancel(ctx)
	errc := make(chan error)
	go func() { errc <- a.Set(wctx, "k", "v2") }()
	cancel()
	require.Error(t, <-errc, "gave up waiting for the backfill; drops k once the stripe is free")

	eventuallyGone(t, b.local, "k", "b handled a's message")
	v, err = b.c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v0", v, "b copied the old value before a's late drop")
	cachex.Settle(b.c)

	close(bl.release)
	cachex.Settle(a) // a's late drop is done
	_, ok := stored(t, shared, "k")
	require.False(t, ok, "a dropped k from the shared layer")

	v, err = a.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v, "a reads the new value")
	assert.Eventually(t, func() bool { v, err := b.c.Get(ctx, "k"); return err == nil && v == "v2" }, 5*time.Second, time.Millisecond,
		"b too, once a's drop reached it")
}
