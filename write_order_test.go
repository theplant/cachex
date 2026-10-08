package cachex

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertMissing(t *testing.T, c Cache[string], key string, msg string) {
	t.Helper()
	v, err := c.Get(context.Background(), key)
	assert.True(t, IsErrKeyNotFound(err), "%s (got %q, %v)", msg, v, err)
}

func TestSetWritesUpstreamBeforeThisLayer(t *testing.T) {
	ctx := context.Background()
	backend := NewSyncMap[string]()
	require.NoError(t, backend.Set(ctx, "k", "old"))
	up := newHookedCache()
	var seenInLayer string
	up.beforeSet = func(key, _ string) error {
		seenInLayer, _ = backend.Get(ctx, key)
		return nil
	}
	cli := NewClient[string](backend, up)

	require.NoError(t, cli.Set(ctx, "k", "new"))
	assert.Equal(t, "old", seenInLayer, "this layer still has the old value while the upstream is written")
	v, _ := backend.Get(ctx, "k")
	assert.Equal(t, "new", v)
	v, _ = up.Get(ctx, "k")
	assert.Equal(t, "new", v)
}

func TestSetUpstreamFailureDropsThisLayersEntry(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	t.Run("cached value", func(t *testing.T) {
		backend := NewSyncMap[string]()
		require.NoError(t, backend.Set(ctx, "k", "old"))
		up := newHookedCache()
		up.beforeSet = func(string, string) error { return boom }
		cli := NewClient[string](backend, up)

		err := cli.Set(ctx, "k", "new")
		require.ErrorIs(t, err, boom)
		assert.Equal(t, "set in upstream failed for key: k: boom", err.Error())
		assertMissing(t, backend, "k", "neither the new value nor the old one stays: the upstream state is unknown")
	})

	t.Run("cached not-found", func(t *testing.T) {
		notFound := NewSyncMap[time.Time]()
		backend := NewSyncMap[string]()
		up := newHookedCache()
		cli := NewClient[string](backend, up, NotFoundWithTTL[string](notFound, time.Hour, 0))
		_, err := cli.Get(ctx, "k")
		require.True(t, IsErrKeyNotFound(err))
		_, err = notFound.Get(ctx, "k")
		require.NoError(t, err, "precondition: not-found is cached")

		up.beforeSet = func(string, string) error { return boom }
		require.ErrorIs(t, cli.Set(ctx, "k", "new"), boom)
		_, err = notFound.Get(ctx, "k")
		assert.True(t, IsErrKeyNotFound(err), "the cached not-found is dropped too, so the next read goes down")
	})
}

func TestDelUpstreamFirst(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	t.Run("deletes the upstream before this layer", func(t *testing.T) {
		backend := NewSyncMap[string]()
		require.NoError(t, backend.Set(ctx, "k", "v"))
		up := newHookedCache()
		var seenInLayer string
		up.beforeDel = func(key string) error {
			seenInLayer, _ = backend.Get(ctx, key)
			return nil
		}
		cli := NewClient[string](backend, up)
		require.NoError(t, cli.Del(ctx, "k"))
		assert.Equal(t, "v", seenInLayer)
		assertMissing(t, backend, "k", "deleted from this layer afterwards")
		require.NoError(t, cli.Del(ctx, "k"), "Del stays idempotent")
	})

	t.Run("upstream failure still drops this layer's entry but caches no not-found", func(t *testing.T) {
		notFound := NewSyncMap[time.Time]()
		backend := NewSyncMap[string]()
		require.NoError(t, backend.Set(ctx, "k", "v"))
		up := newHookedCache()
		up.beforeDel = func(string) error { return boom }
		cli := NewClient[string](backend, up, NotFoundWithTTL[string](notFound, time.Hour, 0))

		err := cli.Del(ctx, "k")
		require.ErrorIs(t, err, boom)
		assert.Equal(t, "delete from upstream failed for key: k: boom", err.Error())
		assertMissing(t, backend, "k", "this layer's value is dropped")
		_, err = notFound.Get(ctx, "k")
		assert.True(t, IsErrKeyNotFound(err), "the upstream may still have the key, so no not-found is cached")
	})
}

// slowSource is a data source whose reads can be held after they have read
// the value, to let a write slip in between the read and the backfill.
type slowSource struct {
	mu      sync.Mutex
	data    map[string]string
	hold    chan struct{} // when non-nil, reads wait on it after reading
	entered chan struct{}
}

func (s *slowSource) read(key string) (string, bool) {
	s.mu.Lock()
	v, ok := s.data[key]
	hold := s.hold
	s.mu.Unlock()
	if hold != nil {
		s.entered <- struct{}{}
		<-hold
	}
	return v, ok
}

func (s *slowSource) Get(_ context.Context, key string) (string, error) {
	if v, ok := s.read(key); ok {
		return v, nil
	}
	return "", &ErrKeyNotFound{}
}

func (s *slowSource) GetMany(_ context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := s.read(k); ok {
			out[k] = v
		}
	}
	return out, nil
}

func (s *slowSource) write(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *slowSource) holdReads() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = make(chan struct{})
	s.entered = make(chan struct{}, 100)
}

func (s *slowSource) releaseReads() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.hold)
	s.hold = nil
}

func TestBackfillDoesNotOverwriteAConcurrentSet(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		read func(cli *Client[string]) (string, error)
	}{
		{"Get", func(cli *Client[string]) (string, error) { return cli.Get(ctx, "k") }},
		{"GetMany", func(cli *Client[string]) (string, error) {
			got, err := cli.GetMany(ctx, []string{"k"})
			return got["k"], err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &slowSource{data: map[string]string{"k": "old"}}
			backend := NewSyncMap[string]()
			cli := NewClient[string](backend, src)

			src.holdReads()
			type res struct {
				v   string
				err error
			}
			done := make(chan res, 1)
			go func() {
				v, err := tc.read(cli)
				done <- res{v, err}
			}()
			<-src.entered // the fetch has read "old"

			src.write("k", "new") // cache-aside: update the source, then the cache
			require.NoError(t, cli.Set(ctx, "k", "new"))
			src.releaseReads()

			r := <-done
			require.NoError(t, r.err)
			assert.Equal(t, "old", r.v, "the read still returns what it fetched")
			v, err := backend.Get(ctx, "k")
			require.NoError(t, err)
			assert.Equal(t, "new", v, "but does not write it over the newer value")
		})
	}

	t.Run("serve-stale refresh", func(t *testing.T) {
		clock := NewMockClock(time.Now())
		defer clock.Install()()
		src := &slowSource{data: map[string]string{"k": "old"}}
		backend := NewSyncMap[*Entry[string]]()
		up := UpstreamFunc[*Entry[string]](func(ctx context.Context, key string) (*Entry[string], error) {
			v, err := src.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			return &Entry[string]{Data: v, CachedAt: NowFunc()}, nil
		})
		cli := NewClient(backend, up,
			EntryWithTTL[string](time.Minute, time.Hour),
			WithServeStale[*Entry[string]](true))

		_, err := cli.Get(ctx, "k")
		require.NoError(t, err)
		clock.Advance(2 * time.Minute)

		src.holdReads()
		v, err := cli.Get(ctx, "k")
		require.NoError(t, err)
		assert.Equal(t, "old", v.Data, "stale value served")
		<-src.entered // the background refresh has read "old"

		src.write("k", "new")
		require.NoError(t, cli.Set(ctx, "k", &Entry[string]{Data: "new", CachedAt: NowFunc()}))
		src.releaseReads()
		waitAsyncRefreshDone(t, cli)

		e, err := backend.Get(ctx, "k")
		require.NoError(t, err)
		assert.Equal(t, "new", e.Data, "the refresh does not overwrite the newer value")
	})

	t.Run("not-found backfill", func(t *testing.T) {
		src := &slowSource{data: map[string]string{}}
		notFound := NewSyncMap[time.Time]()
		backend := NewSyncMap[string]()
		cli := NewClient[string](backend, src, NotFoundWithTTL[string](notFound, time.Hour, 0))

		src.holdReads()
		done := make(chan error, 1)
		go func() {
			_, err := cli.Get(ctx, "k")
			done <- err
		}()
		<-src.entered
		src.write("k", "new")
		require.NoError(t, cli.Set(ctx, "k", "new"))
		src.releaseReads()
		assert.True(t, IsErrKeyNotFound(<-done))

		_, err := notFound.Get(ctx, "k")
		assert.True(t, IsErrKeyNotFound(err), "no not-found is cached over the value just set")
		v, err := cli.Get(ctx, "k")
		require.NoError(t, err)
		assert.Equal(t, "new", v)
	})
}

func TestBackfillIsSerializedWithSet(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		read func(cli *Client[string]) (string, error)
	}{
		{"Get", func(cli *Client[string]) (string, error) { return cli.Get(ctx, "k") }},
		{"GetMany", func(cli *Client[string]) (string, error) {
			got, err := cli.GetMany(ctx, []string{"k"})
			return got["k"], err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &slowSource{data: map[string]string{"k": "old"}}
			backend := newHookedCache()
			cli := NewClient[string](backend, src)

			// The backfill passes its check and is about to write "old" when a
			// Set of "new" starts. Either the Set waits for the backfill, or the
			// backfill lands after the Set returned and a reader sees "old".
			backfillReached := make(chan struct{})
			release := make(chan struct{})
			var first atomic.Bool
			var setReturned atomic.Bool
			var staleAfterSet atomic.Value
			backend.beforeSet = func(_, value string) error {
				if value == "old" && first.CompareAndSwap(false, true) {
					close(backfillReached)
					<-release
				}
				return nil
			}
			backend.afterSet = func(key, value string) {
				if value == "old" && setReturned.Load() {
					v, _ := cli.Get(ctx, key)
					staleAfterSet.Store(v)
				}
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				v, err := tc.read(cli)
				assert.NoError(t, err)
				assert.Equal(t, "old", v)
			}()
			<-backfillReached
			src.write("k", "new")
			setDone := make(chan struct{})
			go func() {
				defer close(setDone)
				assert.NoError(t, cli.Set(ctx, "k", "new"))
				setReturned.Store(true)
			}()
			select {
			case <-setDone:
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			<-done
			<-setDone

			assert.Nil(t, staleAfterSet.Load(), "no reader sees the old value after Set returned")
			v, err := cli.Get(ctx, "k")
			require.NoError(t, err)
			assert.Equal(t, "new", v)
		})
	}
}

// ctxCache is a SyncMap whose Del, like Redis or GORM, refuses a done ctx.
type ctxCache struct{ *SyncMap[string] }

func (c ctxCache) Del(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.SyncMap.Del(ctx, key)
}

func TestFailedWriteInvalidatesWithACanceledCtx(t *testing.T) {
	backend := ctxCache{NewSyncMap[string]()}
	require.NoError(t, backend.Set(context.Background(), "k", "old"))
	up := newHookedCache()
	up.beforeSet = func(string, string) error { return context.Canceled }
	cli := NewClient[string](backend, up)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, cli.Set(ctx, "k", "new"), context.Canceled)
	assertMissing(t, backend, "k", "the cleanup does not reuse the canceled ctx")
}

func TestInterleavedSetsKeepLayersConsistent(t *testing.T) {
	ctx := context.Background()
	for round := range 20 {
		l2Backend := newHookedCache()
		l2Backend.beforeSet = func(string, string) error {
			time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond)
			return nil
		}
		l2 := NewClient[string](l2Backend, UpstreamFunc[string](func(context.Context, string) (string, error) {
			return "", &ErrKeyNotFound{}
		}))
		l1Backend := newHookedCache()
		l1Backend.beforeSet = l2Backend.beforeSet
		l1 := NewClient[string](l1Backend, l2)

		var wg sync.WaitGroup
		for i := range 20 {
			wg.Go(func() {
				if i%5 == 4 {
					_ = l1.Del(ctx, "k")
				} else {
					_ = l1.Set(ctx, "k", fmt.Sprintf("v%d", i))
				}
			})
		}
		wg.Wait()

		v1, err1 := l1Backend.Get(ctx, "k")
		v2, err2 := l2Backend.Get(ctx, "k")
		if err1 == nil {
			require.NoError(t, err2, "round %d: L1 has %q but L2 has nothing", round, v1)
			assert.Equal(t, v2, v1, "round %d: L1 never holds a value L2 does not", round)
		}
		got, err := l1.Get(ctx, "k")
		if err2 == nil {
			require.NoError(t, err)
			assert.Equal(t, v2, got, "round %d", round)
		} else {
			assert.True(t, IsErrKeyNotFound(err), "round %d", round)
		}
	}
}
