package cachex_test

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// gatedSource answers every key with the value it held when asked, after
// the test lets it through: an answer read before a write and returned after
// it.
type gatedSource struct {
	src   *source
	asked chan string
	gate  chan struct{}
	calls atomic.Int64
}

func newGatedSource(data map[string]string) *gatedSource {
	return &gatedSource{src: newSource(data), asked: make(chan string, 16), gate: make(chan struct{})}
}

func (s *gatedSource) Get(ctx context.Context, key string) (string, error) {
	s.calls.Add(1)
	v, err := s.src.Get(ctx, key)
	s.asked <- key
	<-s.gate
	return v, err
}

// instances returns two Caches over one shared leasing layer, as two
// processes would have.
func instances(src cachex.Source[string], opts ...cachex.LayerOption) (a, b *cachex.Cache[string], shared *cachextest.LeasingMap[string]) {
	shared = cachextest.NewLeasingMap[string]()
	layer := func() []cachex.Layer[string] {
		return []cachex.Layer[string]{cachex.NewLayer(shared, append([]cachex.LayerOption{cachex.TTL(time.Minute, time.Minute), cachex.Jitter(0)}, opts...)...)}
	}
	return cachex.New(src, layer()), cachex.New(src, layer()), shared
}

func TestAnAnswerReadBeforeAnotherInstancesWriteIsNotBackfilled(t *testing.T) {
	ctx := context.Background()
	for name, write := range map[string]func(c *cachex.Cache[string]) error{
		"Set": func(c *cachex.Cache[string]) error { return c.Set(ctx, "k", "v2") },
		"Del": func(c *cachex.Cache[string]) error { return c.Del(ctx, "k") },
	} {
		t.Run(name, func(t *testing.T) {
			src := newGatedSource(map[string]string{"k": "v1"})
			a, b, shared := instances(src)
			got := make(chan string)
			go func() {
				v, _ := a.Get(ctx, "k")
				got <- v
			}()
			<-src.asked // a read v1
			src.src.set("k", "v2")
			require.NoError(t, write(b))
			close(src.gate)
			assert.Equal(t, "v1", <-got, "a still answers what it read")
			cachex.Settle(a)
			e, ok := stored(t, shared, "k")
			if name == "Set" {
				require.True(t, ok)
				assert.Equal(t, "v2", e.Value, "b's write stands")
			} else {
				assert.False(t, ok, "b's delete stands")
			}
		})
	}
}

func TestANotFoundReadBeforeAnotherInstancesWriteIsNotBackfilled(t *testing.T) {
	ctx := context.Background()
	src := newGatedSource(nil)
	a, b, shared := instances(src, cachex.NotFoundTTL(time.Minute, 0))
	got := make(chan error)
	go func() {
		_, err := a.Get(ctx, "k")
		got <- err
	}()
	<-src.asked
	src.src.set("k", "created")
	require.NoError(t, b.Set(ctx, "k", "created"))
	close(src.gate)
	require.ErrorIs(t, <-got, cachex.ErrNotFound)
	cachex.Settle(a)
	e, ok := stored(t, shared, "k")
	require.True(t, ok)
	assert.False(t, e.NotFound, "the created key is not hidden")
	assert.Equal(t, "created", e.Value)
}

func TestAMissWhileAnotherInstanceHoldsTheLeaseIsAnsweredButNotBackfilled(t *testing.T) {
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	a, _, shared := instances(src)
	tokens, err := shared.Leaser().Lease(ctx, []string{"k"}, time.Minute) // another instance is fetching k
	require.NoError(t, err)
	require.NotEmpty(t, tokens["k"])
	v, err := a.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v1", v)
	cachex.Settle(a)
	_, ok := stored(t, shared, "k")
	assert.False(t, ok, "the holder of the lease fills the layer")
	require.NoError(t, shared.Leaser().PutLeased(ctx, tokens, map[string]cachex.Entry[string]{"k": {Value: "v1", ExpiresAt: time.Now().Add(time.Hour)}}))
	_, ok = stored(t, shared, "k")
	assert.True(t, ok, "the holder's lease was left alone")
}

func TestALeaseNotUsedForABackfillIsReleased(t *testing.T) {
	ctx := context.Background()
	src := newSource(nil)
	src.fail["k"] = errors.New("boom")
	a, _, shared := instances(src) // no not-found TTL: a not-found deletes, under the lease
	_, err := a.Get(ctx, "k")
	require.Error(t, err)
	cachex.Settle(a)
	tokens, err := shared.Leaser().Lease(ctx, []string{"k", "missing"}, time.Minute)
	require.NoError(t, err)
	assert.NotEmpty(t, tokens["k"], "a failed fetch released its lease")

	_, err = a.Get(ctx, "gone")
	require.ErrorIs(t, err, cachex.ErrNotFound)
	cachex.Settle(a)
	tokens, err = shared.Leaser().Lease(ctx, []string{"gone"}, time.Minute)
	require.NoError(t, err)
	assert.NotEmpty(t, tokens["gone"], "a not-found ended its lease")
}

func TestARefreshSkipsTheSourceWhileAnotherInstanceHoldsTheLease(t *testing.T) {
	ctx := context.Background()
	clock := cachextest.NewClock(time.Now())
	src := newSource(map[string]string{"k": "v2"})
	shared := cachextest.NewLeasingMap[string]()
	c := cachex.New[string](src, []cachex.Layer[string]{cachex.NewLayer(shared, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0))}, cachex.WithNow(clock.Now))
	require.NoError(t, c.Set(ctx, "k", "v1"))
	clock.Advance(2 * time.Minute) // stale
	tokens, err := shared.Leaser().Lease(ctx, []string{"k"}, time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, tokens["k"])

	v, err := c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v1", v, "the stale value")
	cachex.Settle(c)
	assert.Zero(t, src.calls.Load(), "the holder of the lease refreshes it")

	require.NoError(t, shared.Leaser().Release(ctx, tokens))
	_, err = c.Get(ctx, "k")
	require.NoError(t, err)
	cachex.Settle(c)
	assert.Equal(t, int64(1), src.calls.Load(), "once the lease is gone, a stale read refreshes again")
	e, _ := stored(t, shared, "k")
	assert.Equal(t, "v2", e.Value)
}

func TestLeasesAcrossLayers(t *testing.T) {
	// a memory layer over a shared one: the shared layer is leased when it
	// misses, and a key found below fills the layers above under the lease
	ctx := context.Background()
	src := newGatedSource(map[string]string{"k": "v1"})
	newC := func(shared *cachextest.LeasingMap[string]) (*cachex.Cache[string], *cachextest.Map[string]) {
		mem := cachextest.NewMap[string]()
		return cachex.New[string](src, []cachex.Layer[string]{
			cachex.NewLayer(mem, cachex.TTL(time.Minute, 0)),
			cachex.NewLayer(shared, cachex.TTL(time.Hour, 0)),
		}), mem
	}
	shared := cachextest.NewLeasingMap[string]()
	a, aMem := newC(shared)
	b, _ := newC(shared)
	got := make(chan string)
	go func() {
		v, _ := a.Get(ctx, "k")
		got <- v
	}()
	<-src.asked
	src.src.set("k", "v2")
	require.NoError(t, b.Set(ctx, "k", "v2"))
	close(src.gate)
	assert.Equal(t, "v1", <-got)
	cachex.Settle(a)
	e, ok := stored(t, shared, "k")
	require.True(t, ok)
	assert.Equal(t, "v2", e.Value, "the shared layer keeps b's write")
	e, ok = stored(t, aMem, "k")
	require.True(t, ok, "a's own memory layer is filled: only the shared layer leases")
	assert.Equal(t, "v1", e.Value)

	cGetMany, _ := newC(shared)
	require.NoError(t, shared.Del(ctx, "x"))
	src.src.set("x", "vx")
	src.src.set("y", "vy")
	go func() {
		for range 2 {
			<-src.asked
		}
	}()
	m, err := cGetMany.GetMany(ctx, []string{"x", "y"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"x": "vx", "y": "vy"}, m)
	cachex.Settle(cGetMany)
	for _, k := range []string{"x", "y"} {
		_, ok := stored(t, shared, k)
		assert.True(t, ok, "%s: GetMany backfills under its leases", k)
	}
}

// panickyRelease is a leasing map whose leaser panics in Release.
type panickyRelease struct{ *cachextest.LeasingMap[string] }

func (p panickyRelease) Leaser() cachex.Leaser[string] { return panickyLeaser{p.LeasingMap.Leaser()} }

type panickyLeaser struct{ cachex.Leaser[string] }

func (panickyLeaser) Release(context.Context, map[string]string) error { panic("release blew up") }

func TestAPanicInReleaseIsLogged(t *testing.T) {
	// the leases go back after every flight was answered, in the fetch's
	// goroutine: a panic there would crash the process
	src := cachex.SourceFunc[string](func(context.Context, string) (string, error) { return "", errBoom })
	var logs syncBuffer
	c := cachex.New[string](src, oneLayer(panickyRelease{cachextest.NewLeasingMap[string]()}),
		cachex.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	_, err := c.Get(context.Background(), "k")
	require.ErrorIs(t, err, errBoom)
	require.NoError(t, c.Close())
	assert.Contains(t, logs.String(), "release blew up")
}
