package cachex_test

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// logs collects what a Cache logs.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logs) option() cachex.Option {
	return cachex.WithLogger(slog.New(slog.NewTextHandler(l, nil)))
}

func TestFreshness(t *testing.T) {
	ctx := context.Background()

	t.Run("a stale value is served and refreshed in the background", func(t *testing.T) {
		src := newSource(map[string]string{"a": "1"})
		mem := cachextest.NewMap[string]()
		clock, now := newClock()
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(mem, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
		}, now)
		_, err := c.Get(ctx, "a")
		require.NoError(t, err)

		src.set("a", "2")
		src.gate = make(chan struct{}) // holds the refresh until the reads are done
		clock.Advance(2 * time.Minute)
		for range 20 {
			v, err := c.Get(ctx, "a")
			require.NoError(t, err)
			assert.Equal(t, "1", v, "the stale value is returned at once")
		}
		close(src.gate)
		require.NoError(t, c.Close()) // waits for the refresh
		assert.EqualValues(t, 2, src.calls.Load(), "20 stale reads, one refresh")
		e, _ := stored(t, mem, "a")
		assert.Equal(t, "2", e.Value)
		assert.Equal(t, epoch.Add(2*time.Minute), e.CachedAt)

		v, err := c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "2", v)
	})

	t.Run("a rotten value is fetched before returning", func(t *testing.T) {
		src := newSource(map[string]string{"a": "1"})
		clock, now := newClock()
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewMap[string](), cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
		}, now)
		_, err := c.Get(ctx, "a")
		require.NoError(t, err)
		src.set("a", "2")
		clock.Advance(time.Minute + time.Hour)
		v, err := c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "2", v)
	})

	t.Run("without a stale TTL a value is fetched when it turns stale", func(t *testing.T) {
		src := newSource(map[string]string{"a": "1"})
		clock, now := newClock()
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewMap[string](), cachex.TTL(time.Minute, 0), cachex.Jitter(0)),
		}, now)
		_, err := c.Get(ctx, "a")
		require.NoError(t, err)
		src.set("a", "2")
		clock.Advance(time.Minute)
		v, err := c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "2", v)
	})

	t.Run("a stale not-found is served and refreshed without an error log", func(t *testing.T) {
		src := newSource(nil)
		mem := cachextest.NewMap[string]()
		clock, now := newClock()
		var log logs
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(mem, cachex.TTL(time.Minute, 0), cachex.NotFoundTTL(time.Second, time.Minute), cachex.Jitter(0)),
		}, now, log.option())
		_, err := c.Get(ctx, "x")
		require.ErrorIs(t, err, cachex.ErrNotFound)
		clock.Advance(2 * time.Second)
		_, err = c.Get(ctx, "x")
		require.ErrorIs(t, err, cachex.ErrNotFound)
		require.NoError(t, c.Close())
		assert.EqualValues(t, 2, src.calls.Load())
		e, _ := stored(t, mem, "x")
		assert.Equal(t, epoch.Add(2*time.Second), e.CachedAt, "refreshed")
		assert.Empty(t, log.String(), "a key that still does not exist is not a failure")
	})

	t.Run("a failed refresh keeps the stale value and logs", func(t *testing.T) {
		src := newSource(map[string]string{"a": "1"})
		mem := cachextest.NewMap[string]()
		clock, now := newClock()
		var log logs
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(mem, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
		}, now, log.option())
		_, err := c.Get(ctx, "a")
		require.NoError(t, err)
		src.fail["a"] = errBoom
		clock.Advance(2 * time.Minute)
		v, err := c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "1", v)
		require.NoError(t, c.Close())
		e, _ := stored(t, mem, "a")
		assert.Equal(t, "1", e.Value)
		assert.Contains(t, log.String(), "background refresh failed")
	})

	t.Run("after Close stale reads start no refresh", func(t *testing.T) {
		src := newSource(map[string]string{"a": "1"})
		clock, now := newClock()
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewMap[string](), cachex.TTL(time.Minute, time.Hour)),
		}, now)
		_, err := c.Get(ctx, "a")
		require.NoError(t, err)
		require.NoError(t, c.Close())
		clock.Advance(2 * time.Minute)
		v, err := c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "1", v)
		require.NoError(t, c.Close()) // would wait for a refresh, had one started
		assert.EqualValues(t, 1, src.calls.Load())
	})
}

func TestLayers(t *testing.T) {
	ctx := context.Background()
	type setup struct {
		src    *source
		l1, l2 *cachextest.Map[string]
		clock  *cachextest.Clock
		c      *cachex.Cache[string]
	}
	newSetup := func(l1, l2 []cachex.LayerOption) setup {
		s := setup{src: newSource(map[string]string{"a": "1"}), l1: cachextest.NewMap[string](), l2: cachextest.NewMap[string]()}
		var now cachex.Option
		s.clock, now = newClock()
		s.c = cachex.New(s.src, []cachex.Layer[string]{
			cachex.NewLayer(s.l1, append([]cachex.LayerOption{cachex.Jitter(0)}, l1...)...),
			cachex.NewLayer(s.l2, append([]cachex.LayerOption{cachex.Jitter(0)}, l2...)...),
		}, now)
		return s
	}

	t.Run("a miss is backfilled into every layer", func(t *testing.T) {
		s := newSetup([]cachex.LayerOption{cachex.TTL(time.Minute, 0)}, []cachex.LayerOption{cachex.TTL(time.Hour, 0)})
		_, err := s.c.Get(ctx, "a")
		require.NoError(t, err)
		e1, ok1 := stored(t, s.l1, "a")
		e2, ok2 := stored(t, s.l2, "a")
		require.True(t, ok1)
		require.True(t, ok2)
		assert.Equal(t, epoch.Add(time.Minute), e1.ExpiresAt)
		assert.Equal(t, epoch.Add(time.Hour), e2.ExpiresAt)
	})

	t.Run("a hit below is backfilled above, keeping its age", func(t *testing.T) {
		s := newSetup([]cachex.LayerOption{cachex.TTL(time.Minute, 0)}, []cachex.LayerOption{cachex.TTL(time.Hour, 0)})
		require.NoError(t, s.l2.Set(ctx, "a", cachex.Entry[string]{
			Value: "below", CachedAt: epoch, FreshUntil: epoch.Add(time.Hour), ExpiresAt: epoch.Add(time.Hour),
		}))
		s.clock.Advance(30 * time.Second)
		v, err := s.c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "below", v)
		assert.Zero(t, s.src.calls.Load())
		e1, ok := stored(t, s.l1, "a")
		require.True(t, ok)
		assert.Equal(t, epoch, e1.CachedAt, "the age counts from when the source answered")
		assert.Equal(t, epoch.Add(time.Minute), e1.ExpiresAt)
	})

	t.Run("an entry above never outlives the one below", func(t *testing.T) {
		s := newSetup([]cachex.LayerOption{cachex.TTL(time.Hour, time.Hour)}, []cachex.LayerOption{cachex.TTL(time.Minute, 0)})
		require.NoError(t, s.l2.Set(ctx, "a", cachex.Entry[string]{
			Value: "below", CachedAt: epoch, FreshUntil: epoch.Add(time.Minute), ExpiresAt: epoch.Add(time.Minute),
		}))
		_, err := s.c.Get(ctx, "a")
		require.NoError(t, err)
		e1, _ := stored(t, s.l1, "a")
		assert.Equal(t, epoch.Add(time.Minute), e1.FreshUntil)
		assert.Equal(t, epoch.Add(time.Minute), e1.ExpiresAt)
	})

	t.Run("an entry above is never fresher than the one below", func(t *testing.T) {
		s := newSetup([]cachex.LayerOption{cachex.TTL(2*time.Hour, 0)}, []cachex.LayerOption{cachex.TTL(time.Minute, time.Hour)})
		require.NoError(t, s.l2.Set(ctx, "a", cachex.Entry[string]{
			Value: "below", CachedAt: epoch, FreshUntil: epoch.Add(time.Minute), ExpiresAt: epoch.Add(time.Hour),
		}))
		s.clock.Advance(30 * time.Second)
		_, err := s.c.Get(ctx, "a")
		require.NoError(t, err)
		e1, _ := stored(t, s.l1, "a")
		assert.Equal(t, epoch.Add(time.Minute), e1.FreshUntil)
		assert.Equal(t, epoch.Add(time.Hour), e1.ExpiresAt)
	})

	t.Run("a stale entry below is served and refreshed from the source", func(t *testing.T) {
		s := newSetup([]cachex.LayerOption{cachex.TTL(time.Minute, 0)}, []cachex.LayerOption{cachex.TTL(time.Minute, time.Hour)})
		require.NoError(t, s.l2.Set(ctx, "a", cachex.Entry[string]{
			Value: "old", CachedAt: epoch, FreshUntil: epoch.Add(time.Minute), ExpiresAt: epoch.Add(time.Hour),
		}))
		s.clock.Advance(2 * time.Minute)
		v, err := s.c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "old", v)
		require.NoError(t, s.c.Close())
		assert.EqualValues(t, 1, s.src.calls.Load())
		e1, _ := stored(t, s.l1, "a")
		e2, _ := stored(t, s.l2, "a")
		assert.Equal(t, "1", e1.Value)
		assert.Equal(t, "1", e2.Value)
	})

	t.Run("a rotten entry below is fetched", func(t *testing.T) {
		s := newSetup([]cachex.LayerOption{cachex.TTL(time.Minute, 0)}, []cachex.LayerOption{cachex.TTL(time.Minute, 0)})
		require.NoError(t, s.l2.Set(ctx, "a", cachex.Entry[string]{
			Value: "old", CachedAt: epoch, FreshUntil: epoch.Add(time.Minute), ExpiresAt: epoch.Add(time.Minute),
		}))
		s.clock.Advance(time.Minute)
		v, err := s.c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "1", v)
	})

	t.Run("each layer keeps a not-found by its own not-found TTL", func(t *testing.T) {
		s := newSetup(
			[]cachex.LayerOption{cachex.TTL(time.Minute, 0)},
			[]cachex.LayerOption{cachex.TTL(time.Minute, 0), cachex.NotFoundTTL(time.Second, 0)},
		)
		require.NoError(t, s.l1.Set(ctx, "x", cachex.Entry[string]{
			Value: "rotten", CachedAt: epoch.Add(-time.Hour), FreshUntil: epoch.Add(-time.Hour), ExpiresAt: epoch.Add(-time.Hour),
		}))
		_, err := s.c.Get(ctx, "x")
		require.ErrorIs(t, err, cachex.ErrNotFound)
		_, ok := stored(t, s.l1, "x")
		assert.False(t, ok, "a layer without a not-found TTL drops the key")
		e2, ok := stored(t, s.l2, "x")
		require.True(t, ok)
		assert.True(t, e2.NotFound)

		_, err = s.c.Get(ctx, "x")
		require.ErrorIs(t, err, cachex.ErrNotFound)
		assert.EqualValues(t, 1, s.src.calls.Load(), "the second read is answered by the layer below")
	})

	t.Run("a batch source is not asked when a lower layer answers", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1"})
		l2 := cachextest.NewMap[string]()
		var log logs
		c := cachex.New[string](src, []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewMap[string](), cachex.TTL(time.Minute, 0)),
			cachex.NewLayer(l2, cachex.TTL(time.Minute, 0)),
		}, log.option())
		require.NoError(t, l2.Set(ctx, "a", cachex.Entry[string]{Value: "below", FreshUntil: time.Now().Add(time.Hour), ExpiresAt: time.Now().Add(time.Hour)}))
		v, err := c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "below", v)
		assert.Empty(t, src.batchCalls())
		assert.Zero(t, src.calls.Load())
		require.NoError(t, c.Close()) // waits for the fetch to finish
		assert.Empty(t, log.String())
	})

	t.Run("a lower layer that cannot be read fails the read", func(t *testing.T) {
		src := newSource(map[string]string{"a": "1"})
		l2 := newFaulty[string](cachextest.NewMap[string]())
		l2.failOn("get", errBoom)
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewMap[string](), cachex.TTL(time.Minute, 0)),
			cachex.NewLayer(l2, cachex.TTL(time.Minute, 0)),
		})
		_, err := c.Get(ctx, "a")
		require.ErrorIs(t, err, errBoom)
		assert.Zero(t, src.calls.Load())
	})

	t.Run("a failed backfill is logged and the value still returned", func(t *testing.T) {
		src := newSource(map[string]string{"a": "1"})
		l1 := newFaulty[string](cachextest.NewMap[string]())
		l1.failOn("set", errBoom)
		var log logs
		c := cachex.New(src, []cachex.Layer[string]{cachex.NewLayer(l1, cachex.TTL(time.Minute, 0))}, log.option())
		v, err := c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "1", v)
		assert.Contains(t, log.String(), "backfill failed")
	})
}

func TestMaxAgeAndJitter(t *testing.T) {
	ctx := context.Background()

	t.Run("max age caps every layer", func(t *testing.T) {
		src := newSource(map[string]string{"short": "1", "long": "22"})
		l1, l2 := cachextest.NewMap[string](), cachextest.NewMap[string]()
		_, now := newClock()
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(l1, cachex.TTL(time.Minute, time.Minute), cachex.Jitter(0)),
			cachex.NewLayer(l2, cachex.TTL(time.Hour, 0), cachex.Jitter(0)),
		}, now, cachex.WithMaxAge(func(v string) time.Duration { return time.Duration(len(v)) * 10 * time.Second }))
		_, err := c.GetMany(ctx, []string{"short", "long"})
		require.NoError(t, err)

		e1, _ := stored(t, l1, "short")
		e2, _ := stored(t, l2, "short")
		assert.Equal(t, epoch.Add(10*time.Second), e1.ExpiresAt)
		assert.Equal(t, epoch.Add(10*time.Second), e1.FreshUntil)
		assert.Equal(t, epoch.Add(10*time.Second), e2.ExpiresAt)
		e1, _ = stored(t, l1, "long")
		assert.Equal(t, epoch.Add(20*time.Second), e1.ExpiresAt)
	})

	t.Run("a max age of zero is not cached", func(t *testing.T) {
		src := newSource(map[string]string{"a": "1"})
		mem := cachextest.NewMap[string]()
		c := cachex.New(src, []cachex.Layer[string]{cachex.NewLayer(mem, cachex.TTL(time.Minute, 0))},
			cachex.WithMaxAge(func(string) time.Duration { return 0 }))
		for range 2 {
			v, err := c.Get(ctx, "a")
			require.NoError(t, err)
			assert.Equal(t, "1", v)
		}
		assert.EqualValues(t, 2, src.calls.Load())
		assert.Zero(t, mem.Len())
	})

	t.Run("jitter only shortens the fresh period", func(t *testing.T) {
		data := map[string]string{}
		var keys []string
		for i := range 200 {
			k := string(rune('a' + i%26)) + string(rune('A'+i/26))
			data[k], keys = "v", append(keys, k)
		}
		mem := cachextest.NewMap[string]()
		_, now := newClock()
		c := cachex.New(newSource(data), []cachex.Layer[string]{
			cachex.NewLayer(mem, cachex.TTL(100*time.Second, 10*time.Second), cachex.Jitter(0.2)),
		}, now)
		_, err := c.GetMany(ctx, keys)
		require.NoError(t, err)
		distinct := map[time.Time]bool{}
		for _, k := range keys {
			e, _ := stored(t, mem, k)
			fresh := e.FreshUntil.Sub(epoch)
			assert.True(t, fresh > 80*time.Second && fresh <= 100*time.Second, "fresh %v", fresh)
			assert.Equal(t, 10*time.Second, e.ExpiresAt.Sub(e.FreshUntil), "the stale period follows the fresh one")
			distinct[e.FreshUntil] = true
		}
		assert.Greater(t, len(distinct), 100, "entries written together turn stale at different times")
	})
}

func TestCloseWaitsForTheFetchesInProgress(t *testing.T) {
	src := newSource(map[string]string{"a": "1"})
	src.gate = make(chan struct{})
	calls, onGet := started()
	src.onGet = onGet
	mem := cachextest.NewMap[string]()
	c := cachex.New(src, oneLayer(mem))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = c.Get(ctx, "a") }()
	<-calls
	cancel() // the caller leaves; the fetch goes on

	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	require.Never(t, func() bool {
		select {
		case <-closed:
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, time.Millisecond)
	close(src.gate)
	<-closed
	assert.Equal(t, 1, mem.Len(), "the backfill is done before Close returns")
}
