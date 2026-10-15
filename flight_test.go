package cachex_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func oneLayer(b cachex.Backend[string]) []cachex.Layer[string] {
	return []cachex.Layer[string]{cachex.NewLayer(b, cachex.TTL(time.Minute, 0))}
}

// started returns an onGet hook that reports each source call on the channel.
func started() (chan string, func(context.Context, string)) {
	ch := make(chan string, 100)
	return ch, func(_ context.Context, key string) { ch <- key }
}

func TestConcurrentMissesShareOneFetch(t *testing.T) {
	ctx := context.Background()
	src := newSource(map[string]string{"a": "1"})
	src.gate = make(chan struct{})
	calls, onGet := started()
	src.onGet = onGet
	c := cachex.New(src, oneLayer(cachextest.NewMap[string]()))

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			v, err := c.Get(ctx, "a")
			assert.NoError(t, err)
			assert.Equal(t, "1", v)
		})
	}
	wg.Go(func() {
		m, err := c.GetMany(ctx, []string{"a"})
		assert.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1"}, m)
	})
	<-calls
	// no need to wait for the others to join: one that comes after the fetch
	// finds the backfill, or double-checks into it
	close(src.gate)
	wg.Wait()
	assert.EqualValues(t, 1, src.calls.Load())
}

func TestAFetchThatPanicsOrExitsAnswersEveryWaiter(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		do   func()
		want string
	}{
		"panic":  {func() { panic("kaboom") }, "panic during fetch: kaboom"},
		"Goexit": {runtime.Goexit, "fetch exited without returning (runtime.Goexit)"},
	} {
		t.Run(name, func(t *testing.T) {
			var broken atomic.Bool
			broken.Store(true)
			src := newSource(map[string]string{"a": "1"})
			src.onGet = func(context.Context, string) {
				if broken.Load() {
					tc.do()
				}
			}
			var log logs
			c := cachex.New(src, oneLayer(cachextest.NewMap[string]()), log.option())
			var wg sync.WaitGroup
			for range 5 {
				wg.Go(func() {
					_, err := c.Get(ctx, "a")
					assert.ErrorContains(t, err, tc.want)
				})
			}
			wg.Wait()
			if name == "panic" {
				assert.Contains(t, log.String(), "panic during fetch")
				assert.Contains(t, log.String(), "stack=")
			}
			// the waiters wake when the error is published, before the flight
			// is forgotten: a read in between would join it and get the error
			cachex.Settle(c)
			broken.Store(false)
			v, err := c.Get(ctx, "a")
			require.NoError(t, err, "the key is fetched anew")
			assert.Equal(t, "1", v)
		})
	}
}

func TestAFetchOutlivesTheCallerThatStartedIt(t *testing.T) {
	src := newSource(map[string]string{"a": "1"})
	src.gate = make(chan struct{})
	calls, onGet := started()
	var fetchCtx context.Context
	src.onGet = func(ctx context.Context, key string) { fetchCtx = ctx; onGet(ctx, key) }
	mem := cachextest.NewMap[string]()
	c := cachex.New(src, oneLayer(mem), cachex.WithFetchTimeout(time.Minute))

	type ctxKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "trace"))
	done := make(chan error)
	go func() {
		_, err := c.Get(ctx, "a")
		done <- err
	}()
	<-calls
	cancel()
	err := <-done
	require.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, `context done while fetching "a"`)

	assert.NoError(t, fetchCtx.Err(), "the fetch does not end with its caller")
	assert.Equal(t, "trace", fetchCtx.Value(ctxKey{}), "it keeps the caller's values")
	assert.True(t, cachex.IsShared(fetchCtx), "and is marked as shared")
	assert.False(t, cachex.IsShared(ctx))
	deadline, ok := fetchCtx.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, time.Now().Add(time.Minute), deadline, 5*time.Second)

	close(src.gate)
	require.Eventually(t, func() bool { return mem.Len() == 1 }, time.Second, time.Millisecond, "and backfills")
}

func TestFetchTimeout(t *testing.T) {
	src := newSource(map[string]string{"a": "1"})
	src.gate = make(chan struct{}) // never opened: the source waits for its ctx
	c := cachex.New(src, oneLayer(cachextest.NewMap[string]()), cachex.WithFetchTimeout(20*time.Millisecond))
	_, err := c.Get(context.Background(), "a")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ErrorContains(t, err, `get "a" from source`)
}

// slowFirstGet wraps a backend: the first Get of key reads, then waits for
// release before returning what it read.
type slowFirstGet struct {
	cachex.Backend[string]
	once    sync.Once
	read    chan struct{}
	release chan struct{}
}

func (s *slowFirstGet) Get(ctx context.Context, key string) (cachex.Entry[string], bool, error) {
	e, ok, err := s.Backend.Get(ctx, key)
	first := false
	s.once.Do(func() { first = true })
	if first {
		close(s.read)
		<-s.release
	}
	return e, ok, err
}

func TestDoubleCheck(t *testing.T) {
	// A reads a miss; B fetches and backfills the key; only then does A claim
	// the fetch. Re-reading the layer saves A's fetch.
	ctx := context.Background()
	{
		src := newSource(map[string]string{"a": "1"})
		mem := &slowFirstGet{Backend: cachextest.NewMap[string](), read: make(chan struct{}), release: make(chan struct{})}
		c := cachex.New(src, oneLayer(mem))
		done := make(chan struct{})
		go func() {
			defer close(done)
			v, err := c.Get(ctx, "a") // A
			assert.NoError(t, err)
			assert.Equal(t, "1", v)
		}()
		<-mem.read
		_, err := c.Get(ctx, "a") // B
		require.NoError(t, err)
		cachex.Settle(c) // B's fetch is backfilled and gone before A claims
		close(mem.release)
		<-done
		assert.EqualValues(t, 1, src.calls.Load(), "this Cache filled the key since A read it")
	}
}

func TestADoubleCheckOfManyKeysReReadsThemInOneCall(t *testing.T) {
	ctx := context.Background()
	src := newBatchSource(map[string]string{"a": "src", "b": "src", "c": "src"})
	mem := cachextest.NewMap[string]()
	var gets atomic.Int64
	var c *cachex.Cache[string]
	layer := hook(mem, func(op string, keys []string, many bool) {
		if op != "get" {
			return
		}
		switch gets.Add(1) {
		case 1: // this Cache writes a and b while the first read is out, so they are re-read
			require.NoError(t, c.DelMany(ctx, []string{"a", "b"}))
		case 2:
			assert.True(t, many, "the re-read of %v is one call", keys)
			// another process filled a and b between the read and the re-read
			for _, key := range []string{"a", "b"} {
				require.NoError(t, mem.Set(ctx, key, cachex.Entry[string]{Value: "other", FreshUntil: time.Now().Add(time.Hour), ExpiresAt: time.Now().Add(time.Hour)}))
			}
		}
	})
	c = cachex.New[string](src, oneLayer(layer))
	m, err := c.GetMany(ctx, []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "other", "b": "other", "c": "src"}, m)
	assert.Empty(t, src.batchCalls())
	assert.EqualValues(t, 1, src.calls.Load(), "only c is left for the source")
}

func TestDoubleCheckSkipsTheReReadWhenNothingWasWritten(t *testing.T) {
	src := newSource(map[string]string{"a": "1"})
	var reads atomic.Int64
	c := cachex.New(src, oneLayer(counting(cachextest.NewMap[string](), "get", &reads)))
	_, err := c.Get(context.Background(), "a")
	require.NoError(t, err)
	assert.EqualValues(t, 1, reads.Load(), "a cold miss reads the layer once")
}

// errCallback is what a source gets back when it calls the Cache for a key
// its own fetch is fetching.
func requireCallbackError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "called back into the same Cache")
}

func TestASourceCallingBackForItsOwnKeyFailsAtOnce(t *testing.T) {
	var c *cachex.Cache[string]
	var inner error
	c = cachex.New[string](cachex.SourceFunc[string](func(ctx context.Context, key string) (string, error) {
		_, inner = c.Get(ctx, key)
		return "v", nil
	}), []cachex.Layer[string]{cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Minute, 0))},
		cachex.WithFetchTimeout(time.Hour))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Get(context.Background(), "a")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the source's call for its own key waited for itself")
	}
	requireCallbackError(t, inner)
}

func TestASourceCallingBackForAKeyWaitingItsTurnAsksForIt(t *testing.T) {
	var c *cachex.Cache[string]
	var calls sync.Map
	c = cachex.New[string](cachex.SourceFunc[string](func(ctx context.Context, key string) (string, error) {
		n, _ := calls.LoadOrStore(key, new(atomic.Int64))
		n.(*atomic.Int64).Add(1)
		if key == "a" {
			v, err := c.Get(ctx, "b") // b waits for a's turn: GetMany asks one key at a time
			return "a+" + v, err
		}
		return key, nil
	}), []cachex.Layer[string]{cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Minute, 0))},
		cachex.WithFetchTimeout(time.Hour), cachex.WithGetManyConcurrency(1))
	type res struct {
		got map[string]string
		err error
	}
	done := make(chan res, 1)
	go func() {
		got, err := c.GetMany(context.Background(), []string{"a", "b"})
		done <- res{got, err}
	}()
	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, map[string]string{"a": "a+b", "b": "b"}, r.got)
		n, _ := calls.Load("b")
		assert.Equal(t, int64(1), n.(*atomic.Int64).Load(), "b asked once, by a's source, in a's turn")
	case <-time.After(5 * time.Second):
		t.Fatal("a's source waited for b, which waited for a's turn")
	}
}

func TestASourceCallingBackForAKeyBeingAskedWaitsForIt(t *testing.T) {
	var c *cachex.Cache[string]
	aCalling := make(chan struct{})
	c = cachex.New[string](cachex.SourceFunc[string](func(ctx context.Context, key string) (string, error) {
		if key == "a" {
			close(aCalling)
			v, err := c.Get(ctx, "b") // b is being asked at the same time
			return "a+" + v, err
		}
		<-aCalling // b answers only after a started waiting for it
		return key, nil
	}), []cachex.Layer[string]{cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Minute, 0))},
		cachex.WithFetchTimeout(time.Hour), cachex.WithGetManyConcurrency(2))
	got, err := c.GetMany(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "a+b", "b": "b"}, got)
}

func TestACallbackCycleFailsAtOnce(t *testing.T) {
	var c *cachex.Cache[string]
	c = cachex.New[string](cachex.SourceFunc[string](func(ctx context.Context, key string) (string, error) {
		v, err := c.Get(ctx, map[string]string{"a": "b", "b": "a"}[key]) // a needs b, b needs a
		return key + v, err
	}), []cachex.Layer[string]{cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Minute, 0))},
		cachex.WithFetchTimeout(time.Hour))
	done := make(chan error, 1)
	go func() { _, err := c.Get(context.Background(), "a"); done <- err }()
	select {
	case err := <-done:
		requireCallbackError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a's source waited for b, whose source waited for a")
	}
}

// batchFunc adapts a function to a BatchSource (and a Source of one key).
type batchFunc func(ctx context.Context, keys []string) (map[string]string, error)

func (f batchFunc) Get(ctx context.Context, key string) (string, error) {
	m, err := f(ctx, []string{key})
	if err != nil {
		return "", err
	}
	if v, ok := m[key]; ok {
		return v, nil
	}
	return "", cachex.ErrNotFound
}

func (f batchFunc) GetMany(ctx context.Context, keys []string) (map[string]string, error) {
	return f(ctx, keys)
}

func TestABatchSourceCallingBackForAKeyOfItsCallFailsAtOnce(t *testing.T) {
	var c *cachex.Cache[string]
	var inner error
	c = cachex.New[string](batchFunc(func(ctx context.Context, keys []string) (map[string]string, error) {
		if len(keys) > 1 {
			_, inner = c.Get(ctx, keys[1]) // asked in this very call
		}
		out := map[string]string{}
		for _, k := range keys {
			out[k] = k
		}
		return out, nil
	}), []cachex.Layer[string]{cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Minute, 0))},
		cachex.WithFetchTimeout(time.Hour))
	done := make(chan struct{})
	go func() { defer close(done); _, _ = c.GetMany(context.Background(), []string{"a", "b"}) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the batch source's call for a key of its own call waited for itself")
	}
	requireCallbackError(t, inner)
}

func TestASourceCanReadAnotherKeyOfItsCache(t *testing.T) {
	var c *cachex.Cache[string]
	c = cachex.New[string](cachex.SourceFunc[string](func(ctx context.Context, key string) (string, error) {
		if key == "a" {
			v, err := c.Get(ctx, "b") // not being fetched: fetched on its own
			return "a+" + v, err
		}
		return key, nil
	}), []cachex.Layer[string]{cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Minute, 0))})
	v, err := c.Get(context.Background(), "a")
	require.NoError(t, err)
	assert.Equal(t, "a+b", v)
}
