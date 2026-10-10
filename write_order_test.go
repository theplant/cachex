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

func TestLayerWriteFailureAfterUpstreamSuccess(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	t.Run("Set", func(t *testing.T) {
		notFound := NewSyncMap[time.Time]()
		require.NoError(t, notFound.Set(ctx, "k", time.Now()))
		backend := newHookedCache()
		require.NoError(t, backend.Set(ctx, "k", "old"))
		backend.beforeSet = func(string, string) error { return boom }
		up := newHookedCache()
		cli := NewClient[string](backend, up, NotFoundWithTTL[string](notFound, time.Hour, 0))

		err := cli.Set(ctx, "k", "new")
		require.ErrorIs(t, err, boom)
		assert.Equal(t, "set in backend failed for key: k: boom", err.Error())
		v, err := up.Get(ctx, "k")
		require.NoError(t, err)
		assert.Equal(t, "new", v, "the upstream keeps the write")
		assertMissing(t, backend, "k", "this layer's old value is dropped")
		_, err = notFound.Get(ctx, "k")
		assert.True(t, IsErrKeyNotFound(err), "the cached not-found is dropped")
	})

	t.Run("Del", func(t *testing.T) {
		notFound := NewSyncMap[time.Time]()
		backend := newHookedCache()
		require.NoError(t, backend.Set(ctx, "k", "old"))
		var failed atomic.Bool
		backend.beforeDel = func(string) error {
			if failed.CompareAndSwap(false, true) {
				return boom // only this layer's own Del fails, not the cleanup
			}
			return nil
		}
		up := newHookedCache()
		require.NoError(t, up.Set(ctx, "k", "old"))
		cli := NewClient[string](backend, up, NotFoundWithTTL[string](notFound, time.Hour, 0))

		err := cli.Del(ctx, "k")
		require.ErrorIs(t, err, boom)
		assert.Equal(t, "delete from backend failed for key: k: boom", err.Error())
		assertMissing(t, up, "k", "the upstream keeps the delete")
		assertMissing(t, backend, "k", "this layer's old value is dropped")
		_, err = notFound.Get(ctx, "k")
		assert.True(t, IsErrKeyNotFound(err), "the not-found written before the failure is dropped too")
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
	ctx, cancel := context.WithCancel(context.Background())
	up := newHookedCache()
	up.beforeSet = func(string, string) error {
		cancel() // the caller gives up while the upstream is being written
		return context.Canceled
	}
	cli := NewClient[string](backend, up)

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

func TestReadAfterWriteDoesNotJoinAnOlderFetch(t *testing.T) {
	for _, conc := range []int{1, 4} {
		for _, many := range []bool{false, true} {
			t.Run(fmt.Sprintf("concurrency=%d GetMany=%v", conc, many), func(t *testing.T) {
				ctx := context.Background()
				src := &slowSource{data: map[string]string{"k": "old"}}
				// no not-found cache: Del leaves this layer empty
				cli := NewClient[string](NewSyncMap[string](), src, WithFetchConcurrency[string](conc))

				// older reads hold a fetch in every slot, each having read "old"
				src.holdReads()
				require.Eventually(t, func() bool {
					go func() { _, _ = cli.Get(ctx, "k") }()
					return cli.flights.Len() == conc
				}, time.Second, time.Millisecond)
				for range conc {
					waitFor(t, src.entered)
				}

				src.mu.Lock()
				delete(src.data, "k")
				src.mu.Unlock()
				require.NoError(t, cli.Del(ctx, "k"))

				got := make(chan error, 1)
				go func() {
					if many {
						m, err := cli.GetMany(ctx, []string{"k"})
						if err == nil && len(m) > 0 {
							err = fmt.Errorf("got %v", m)
						}
						got <- err
						return
					}
					v, err := cli.Get(ctx, "k")
					if err == nil {
						err = fmt.Errorf("got %q", v)
					}
					got <- err
				}()
				waitFor(t, src.entered) // the new read started its own fetch instead of joining an older one
				src.releaseReads()
				err := <-got
				if many {
					assert.NoError(t, err, "a GetMany after Del sees the delete")
				} else {
					assert.True(t, IsErrKeyNotFound(err), "a Get after Del sees the delete: %v", err)
				}
			})
		}
	}
}

func TestWriteWaitingForTheStripeRespectsCtx(t *testing.T) {
	ctx := context.Background()
	up := newHookedCache()
	entered, release := make(chan struct{}), make(chan struct{})
	up.beforeSet = func(_, value string) error {
		if value == "first" {
			close(entered)
			<-release
		}
		return nil
	}
	cli := NewClient[string](NewSyncMap[string](), up)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		assert.NoError(t, cli.Set(ctx, "k", "first"))
	}()
	waitFor(t, entered)

	secondErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		secondErr <- cli.Set(ctx, "k", "second")
	}()
	select {
	case err := <-secondErr:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, "context cancelled while waiting to write key: k: context deadline exceeded", err.Error())
	case <-time.After(5 * time.Second):
		t.Fatal("Set kept waiting past its ctx")
	}
	close(release)
	<-firstDone
	v, _ := up.Get(ctx, "k")
	assert.Equal(t, "first", v, "the cancelled Set wrote nothing")

	s := cli.stripe("k")
	// the given-up Set's lock is taken in the background once the first Set
	// releases it; it bumps the generation (the first Set did too) and unlocks
	require.Eventually(t, func() bool { return s.Generation() == 2 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		if !s.TryLock() {
			return false
		}
		s.Unlock()
		return true
	}, time.Second, time.Millisecond)
	done, cancel := context.WithCancel(ctx)
	cancel()
	require.NoError(t, cli.Set(done, "k", "third"), "a done ctx only gives up waiting; a free stripe is written as before")
	v, _ = up.Get(ctx, "k")
	assert.Equal(t, "third", v)
}

func TestWriteWaitingForABackfillRespectsCtx(t *testing.T) {
	src := &slowSource{data: map[string]string{"k": "old"}}
	backend := newHookedCache()
	cli := NewClient[string](backend, src)
	reached, release := make(chan struct{}), make(chan struct{})
	backend.beforeSet = func(_, v string) error {
		if v == "old" { // the GetMany backfill, holding the stripe for reading
			close(reached)
			<-release
		}
		return nil
	}
	getDone := make(chan struct{})
	go func() {
		defer close(getDone)
		_, _ = cli.GetMany(context.Background(), []string{"k"})
	}()
	waitFor(t, reached)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- cli.Set(ctx, "k", "new") }()
	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Error("Set kept waiting past its ctx")
	}
	close(release)
	<-getDone

	require.Eventually(t, func() bool {
		_, err := backend.Get(context.Background(), "k")
		return IsErrKeyNotFound(err)
	}, time.Second, time.Millisecond, "the given-up Set drops this layer's entry once the stripe is free")
	v, err := src.Get(context.Background(), "k")
	require.NoError(t, err)
	assert.Equal(t, "old", v, "and writes nothing upstream")
	require.NoError(t, cli.Set(context.Background(), "k", "new"), "the stripe is free again")
}

func TestDelWithADoneCtxStillInvalidates(t *testing.T) {
	backend := NewSyncMap[string]()
	db := map[string]string{"k": "v1"}
	var mu sync.Mutex
	cli := NewClient[string](backend, UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if v, ok := db[key]; ok {
			return v, nil
		}
		return "", &ErrKeyNotFound{}
	}))
	_, err := cli.Get(context.Background(), "k")
	require.NoError(t, err)

	// cache-aside: the DB is updated, then the request's ctx is canceled before the invalidation
	mu.Lock()
	db["k"] = "v2"
	mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, cli.Del(ctx, "k"))
	v, err := cli.Get(context.Background(), "k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v)
}

// holdGetCache is a hookedCache whose Get can be held after reading.
type holdGetCache struct {
	*hookedCache
	onGet func(key string)
}

func (h *holdGetCache) Get(ctx context.Context, key string) (string, error) {
	v, err := h.hookedCache.Get(ctx, key)
	if h.onGet != nil {
		h.onGet(key)
	}
	return v, err
}

func TestWriteThroughBackfillDuringUpstreamWrite(t *testing.T) {
	ctx := context.Background()
	l2 := &holdGetCache{hookedCache: newHookedCache()}
	require.NoError(t, l2.Set(ctx, "k", "old"))
	l1Backend := NewSyncMap[string]()
	l1 := NewClient[string](l1Backend, l2)

	read, release, getDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	l2.beforeSet = func(string, string) error {
		// while L1's Set holds the stripe and before L2 is written, an L1 Get reads L2's old value
		l2.onGet = func(string) { close(read); <-release }
		go func() {
			defer close(getDone)
			_, _ = l1.Get(ctx, "k")
		}()
		<-read
		return nil
	}
	require.NoError(t, l1.Set(ctx, "k", "new"))
	l2.beforeSet = nil
	close(release)
	<-getDone

	v, err := l1Backend.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "new", v, "the Get that read L2 before the write does not backfill the old value")
}

func TestGivenUpDelStillInvalidates(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	db := map[string]string{"k": "v1"}
	backend := newHookedCache()
	cli := NewClient[string](backend, UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if v, ok := db[key]; ok {
			return v, nil
		}
		return "v-" + key, nil
	}))
	_, err := cli.Get(ctx, "k")
	require.NoError(t, err)

	other := "" // an unrelated key in k's stripe
	for i := 0; other == ""; i++ {
		if k := fmt.Sprintf("o%d", i); cli.stripe(k) == cli.stripe("k") {
			other = k
		}
	}
	reached, release := make(chan struct{}), make(chan struct{})
	backend.beforeSet = func(key, _ string) error {
		if key == other { // its backfill holds the stripe for reading
			close(reached)
			<-release
		}
		return nil
	}
	getDone := make(chan struct{})
	go func() {
		defer close(getDone)
		_, _ = cli.Get(ctx, other)
	}()
	waitFor(t, reached)

	// cache-aside: the DB changes, then the request's ctx is canceled before the invalidation
	mu.Lock()
	db["k"] = "v2"
	mu.Unlock()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, cli.Del(canceled, "k"), context.Canceled, "the stripe is busy, so the Del gives up waiting")
	close(release)
	<-getDone

	require.Eventually(t, func() bool {
		v, err := cli.Get(ctx, "k")
		return err == nil && v == "v2"
	}, time.Second, time.Millisecond, "the old value is dropped once the stripe is free")
}

func TestReadAfterDelDoesNotJoinAFetchStartedDuringIt(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	db := map[string]string{"k": "v1"}
	var armed atomic.Bool
	r0Read, releaseR0 := make(chan struct{}), make(chan struct{})
	backend := &holdGetCache{hookedCache: newHookedCache()}
	backend.onGet = func(string) {
		if armed.CompareAndSwap(true, false) { // R0's double-check, after reading "v1"
			close(r0Read)
			<-releaseR0
		}
	}
	cli := NewClient[string](backend, UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if v, ok := db[key]; ok {
			return v, nil
		}
		return "", &ErrKeyNotFound{}
	}), WithDoubleCheck[string](DoubleCheckEnabled))

	var starts atomic.Int32
	r0AtClaim, r0Go := make(chan struct{}), make(chan struct{})
	cli.testHooks = &testHooks{beforeSingleflightStart: func(context.Context, string) {
		if starts.Add(1) == 1 { // R0 missed before anything was cached; hold it before it claims
			close(r0AtClaim)
			<-r0Go
		}
	}}
	go func() { _, _ = cli.Get(ctx, "k") }() // R0
	waitFor(t, r0AtClaim)
	v, err := cli.Get(ctx, "k") // R1 caches "v1"
	require.NoError(t, err)
	require.Equal(t, "v1", v)

	mu.Lock()
	delete(db, "k")
	mu.Unlock()
	backend.beforeDel = func(string) error {
		// between the upstream write and this layer's delete, R0 claims a fetch
		// whose double-check reads "v1" from this layer
		armed.Store(true)
		close(r0Go)
		<-r0Read
		return nil
	}
	require.NoError(t, cli.Del(ctx, "k"))
	backend.beforeDel = nil

	got := make(chan error, 1)
	go func() { // R2 starts after Del returned
		v, err := cli.Get(ctx, "k")
		if err == nil {
			err = fmt.Errorf("got %q", v)
		}
		got <- err
	}()
	var r2 error
	select {
	case r2 = <-got:
		close(releaseR0)
	case <-time.After(time.Second): // R2 joined R0's fetch
		close(releaseR0)
		r2 = <-got
	}
	assert.True(t, IsErrKeyNotFound(r2), "a Get after Del returned sees the delete: %v", r2)
}

// jitterCache is a Cache with random latency, to shake out interleavings.
type jitterCache struct{ m *SyncMap[string] }

func jitter() { time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond) }

func (j jitterCache) Get(ctx context.Context, key string) (string, error) {
	jitter()
	v, err := j.m.Get(ctx, key)
	jitter() // between reading and returning: where a write can slip in before the backfill
	jitter()
	jitter()
	return v, err
}

func (j jitterCache) Set(ctx context.Context, key, value string) error {
	jitter()
	return j.m.Set(ctx, key, value)
}

func (j jitterCache) Del(ctx context.Context, key string) error {
	jitter()
	return j.m.Del(ctx, key)
}

// TestRandomInterleavingsNeverReadOlderThanACompletedWrite: one writer per
// key Sets and Dels versions 1, 2, 3...; readers Get and GetMany through two
// layers. A read that starts after write n returned must see version n or a
// later one that had started (a Del is the version "absent").
func TestRandomInterleavingsNeverReadOlderThanACompletedWrite(t *testing.T) {
	for _, notFound := range []bool{false, true} {
		for _, dc := range []DoubleCheckMode{DoubleCheckDisabled, DoubleCheckEnabled} {
			t.Run(fmt.Sprintf("notFoundCache=%v doubleCheck=%v", notFound, dc == DoubleCheckEnabled), func(t *testing.T) {
				testRandomInterleavings(t, notFound, dc)
			})
		}
	}
}

func testRandomInterleavings(t *testing.T, withNotFound bool, dc DoubleCheckMode) {
	ctx := context.Background()
	keys := []string{"a", "b"}
	type history struct {
		mu      sync.Mutex
		isDel   []bool // by version; version 0 is the initial "absent"
		started int
		done    int
	}
	hist := map[string]*history{}
	for _, k := range keys {
		hist[k] = &history{isDel: []bool{true}}
	}

	src := jitterCache{m: NewSyncMap[string]()}
	l2 := NewClient[string](jitterCache{m: NewSyncMap[string]()}, src)
	opts := []ClientOption[string]{WithDoubleCheck[string](dc)}
	if withNotFound {
		opts = append(opts, NotFoundWithTTL[string](NewSyncMap[time.Time](), time.Hour, 0))
	}
	l1 := NewClient[string](NewSyncMap[string](), l2, opts...)

	// check reports whether what a read saw for key is allowed, given the
	// version completed before it started.
	check := func(key string, floor int, v string, found bool) error {
		h := hist[key]
		h.mu.Lock()
		defer h.mu.Unlock()
		if found {
			var n int
			if _, err := fmt.Sscanf(v, key+":%d", &n); err != nil || n < floor || n > h.started || h.isDel[n] {
				return fmt.Errorf("%s: got %q, but write %d had completed before the read", key, v, floor)
			}
			return nil
		}
		for n := floor; n <= h.started; n++ {
			if h.isDel[n] {
				return nil
			}
		}
		return fmt.Errorf("%s: got absent, but write %d (a Set) had completed and no later Del had started", key, floor)
	}

	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Go(func() {
			h := hist[k]
			for n := 1; n <= 300; n++ {
				del := rand.IntN(2) == 0
				h.mu.Lock()
				h.isDel = append(h.isDel, del)
				h.started = n
				h.mu.Unlock()
				var err error
				if del {
					err = l1.Del(ctx, k)
				} else {
					err = l1.Set(ctx, k, fmt.Sprintf("%s:%d", k, n))
				}
				if !assert.NoError(t, err) {
					return
				}
				h.mu.Lock()
				h.done = n
				h.mu.Unlock()
			}
		})
	}
	var violations atomic.Int32
	for range 12 {
		wg.Go(func() {
			for range 400 {
				floors := map[string]int{}
				for _, k := range keys {
					hist[k].mu.Lock()
					floors[k] = hist[k].done
					hist[k].mu.Unlock()
				}
				if rand.IntN(2) == 0 {
					k := keys[rand.IntN(len(keys))]
					v, err := l1.Get(ctx, k)
					if err != nil && !IsErrKeyNotFound(err) {
						t.Errorf("Get(%s): %v", k, err)
						return
					}
					if err := check(k, floors[k], v, err == nil); err != nil {
						violations.Add(1)
						t.Error(err)
					}
					continue
				}
				got, err := l1.GetMany(ctx, keys)
				if err != nil {
					t.Errorf("GetMany: %v", err)
					return
				}
				for _, k := range keys {
					v, ok := got[k]
					if err := check(k, floors[k], v, ok); err != nil {
						violations.Add(1)
						t.Error(err)
					}
				}
			}
		})
	}
	wg.Wait()
	require.Zero(t, violations.Load())

	for _, k := range keys { // quiesced: every layer agrees with the source
		want, wantErr := src.Get(ctx, k)
		got, err := l1.Get(ctx, k)
		assert.Equal(t, IsErrKeyNotFound(wantErr), IsErrKeyNotFound(err), k)
		assert.Equal(t, want, got, k)
	}
}
