package cachex_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// recorder wraps a backend and appends "name op key" for every write.
type recorder struct {
	cachex.Backend[string]
	name string
	mu   *sync.Mutex
	log  *[]string
}

func (r *recorder) note(op string, keys ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range keys {
		*r.log = append(*r.log, r.name+" "+op+" "+k)
	}
}

func (r *recorder) Set(ctx context.Context, key string, e cachex.Entry[string]) error {
	r.note("set", key)
	return r.Backend.Set(ctx, key, e)
}

func (r *recorder) SetMany(ctx context.Context, entries map[string]cachex.Entry[string]) error {
	for k := range entries {
		r.note("set", k)
	}
	return r.Backend.SetMany(ctx, entries)
}

func (r *recorder) Del(ctx context.Context, key string) error {
	r.note("del", key)
	return r.Backend.Del(ctx, key)
}

func (r *recorder) DelMany(ctx context.Context, keys []string) error {
	r.note("del", keys...)
	return r.Backend.DelMany(ctx, keys)
}

type twoLayers struct {
	src        *source
	l1, l2     *faulty[string]
	m1, m2     *cachextest.Map[string]
	c          *cachex.Cache[string]
	mu         sync.Mutex
	log        []string
	clockLayer *cachextest.Clock
}

func newTwoLayers(t *testing.T) *twoLayers {
	t.Helper()
	s := &twoLayers{src: newSource(map[string]string{"a": "1", "b": "1"}), m1: cachextest.NewMap[string](), m2: cachextest.NewMap[string]()}
	s.l1 = newFaulty[string](&recorder{Backend: s.m1, name: "l1", mu: &s.mu, log: &s.log})
	s.l2 = newFaulty[string](&recorder{Backend: s.m2, name: "l2", mu: &s.mu, log: &s.log})
	var now cachex.Option
	s.clockLayer, now = newClock()
	s.c = cachex.New(s.src, []cachex.Layer[string]{
		cachex.NewLayer(s.l1, cachex.TTL(time.Minute, 0), cachex.Jitter(0)),
		cachex.NewLayer(s.l2, cachex.TTL(time.Hour, 0), cachex.Jitter(0)),
	}, now)
	return s
}

func (s *twoLayers) writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.log
	s.log = nil
	return out
}

func TestSetWritesEveryLayerBottomUp(t *testing.T) {
	ctx := context.Background()
	s := newTwoLayers(t)
	require.NoError(t, s.c.Set(ctx, "a", "new"))
	assert.Equal(t, []string{"l2 set a", "l1 set a"}, s.writes())

	e1, _ := stored(t, s.m1, "a")
	e2, _ := stored(t, s.m2, "a")
	assert.Equal(t, cachex.Entry[string]{Value: "new", CachedAt: epoch, FreshUntil: epoch.Add(time.Minute), ExpiresAt: epoch.Add(time.Minute)}, e1)
	assert.Equal(t, epoch.Add(time.Hour), e2.ExpiresAt)

	v, err := s.c.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "new", v)
	assert.Zero(t, s.src.calls.Load(), "Set does not write the source")
}

func TestDelInvalidatesEveryLayerBottomUp(t *testing.T) {
	ctx := context.Background()
	s := newTwoLayers(t)
	_, err := s.c.Get(ctx, "a")
	require.NoError(t, err)
	s.writes()

	s.src.set("a", "2") // an update, then Del to invalidate
	require.NoError(t, s.c.Del(ctx, "a"))
	assert.Equal(t, []string{"l2 del a", "l1 del a"}, s.writes())
	v, err := s.c.Get(ctx, "a")
	require.NoError(t, err, "Del does not record a not-found")
	assert.Equal(t, "2", v)
}

func TestAFailedWriteLeavesNoEntryAtOrAboveTheFailedLayer(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, op, layer string
	}{
		{"Set fails below", "set", "l2"},
		{"Set fails above", "set", "l1"},
		{"Del fails below", "del", "l2"},
		{"Del fails above", "del", "l1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTwoLayers(t)
			_, err := s.c.Get(ctx, "a") // both layers hold "1"
			require.NoError(t, err)
			failing := map[string]*faulty[string]{"l1": s.l1, "l2": s.l2}[tc.layer]
			failing.failOn(tc.op, errBoom)
			if tc.op == "set" {
				err = s.c.Set(ctx, "a", "new")
			} else {
				err = s.c.Del(ctx, "a")
			}
			require.ErrorIs(t, err, errBoom)
			failing.failOn(tc.op, nil)

			_, in1 := stored(t, s.m1, "a")
			e2, in2 := stored(t, s.m2, "a")
			if tc.layer == "l1" && tc.op == "del" {
				// a layer that cannot delete keeps its entry; the one below is gone
				assert.False(t, in2)
				return
			}
			assert.False(t, in1, "the layer above never keeps an entry the write may have changed below")
			switch {
			case tc.layer == "l2" && tc.op == "set":
				assert.False(t, in2, "the failed layer is invalidated too")
			case tc.layer == "l1" && tc.op == "set":
				assert.Equal(t, "new", e2.Value, "the layer below keeps what was written")
			}
		})
	}
}

func TestAFetchStartedBeforeAWriteNeverOverwritesIt(t *testing.T) {
	ctx := context.Background()
	s := newTwoLayers(t)
	s.src.gate = make(chan struct{})
	calls, onGet := started()
	s.src.onGet = onGet

	got := make(chan string)
	go func() {
		v, err := s.c.Get(ctx, "a") // reads the source's "1", then waits at the gate
		assert.NoError(t, err)
		got <- v
	}()
	<-calls
	require.NoError(t, s.c.Set(ctx, "a", "new"))
	close(s.src.gate)
	assert.Equal(t, "1", <-got, "a read that started before the write may return the old value")

	for _, m := range []*cachextest.Map[string]{s.m1, s.m2} {
		e, ok := stored(t, m, "a")
		require.True(t, ok)
		assert.Equal(t, "new", e.Value, "but its backfill does not land")
	}
	v, err := s.c.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "new", v)
}

func TestAReadAfterAWriteNeverJoinsAFetchStartedBeforeIt(t *testing.T) {
	ctx := context.Background()
	src := newSource(map[string]string{"a": "1"})
	src.gate = make(chan struct{})
	calls, onGet := started()
	src.onGet = onGet
	c := cachex.New[string](src, nil) // no layer: only the flight could carry the old value
	go func() { _, _ = c.Get(ctx, "a") }()
	<-calls
	src.set("a", "2")
	require.NoError(t, c.Del(ctx, "a"))
	go func() { <-calls; close(src.gate) }()
	v, err := c.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "2", v)
}

// blockingSet wraps a backend: Set of key "a" waits for release.
type blockingSet struct {
	cachex.Backend[string]
	entered chan struct{}
	release chan struct{}
}

func (b *blockingSet) SetMany(ctx context.Context, entries map[string]cachex.Entry[string]) error {
	if _, ok := entries["a"]; ok {
		b.entered <- struct{}{}
		<-b.release
	}
	return b.Backend.SetMany(ctx, entries)
}

func TestAWriteThatGivesUpWaitingStillInvalidates(t *testing.T) {
	ctx := context.Background()
	mem := cachextest.NewMap[string]()
	bs := &blockingSet{Backend: mem, entered: make(chan struct{}, 1), release: make(chan struct{})}
	c := cachex.New(newSource(nil), oneLayer(bs))

	first := make(chan error)
	go func() { first <- c.Set(ctx, "a", "first") }()
	<-bs.entered

	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err := c.Del(waitCtx, "a") // waits for the first Set's stripe, then gives up
	require.ErrorIs(t, err, context.DeadlineExceeded)

	close(bs.release)
	require.NoError(t, <-first)
	require.Eventually(t, func() bool { return mem.Len() == 0 }, time.Second, time.Millisecond,
		"the caller may have changed the source already: the key is invalidated once the stripe is free")
}

func TestSetManyAndDelMany(t *testing.T) {
	ctx := context.Background()

	t.Run("write every layer bottom up", func(t *testing.T) {
		s := newTwoLayers(t)
		require.NoError(t, s.c.SetMany(ctx, map[string]string{"a": "A", "b": "B"}))
		w := s.writes()
		require.Len(t, w, 4)
		assert.ElementsMatch(t, []string{"l2 set a", "l2 set b"}, w[:2])
		assert.ElementsMatch(t, []string{"l1 set a", "l1 set b"}, w[2:])
		m, err := s.c.GetMany(ctx, []string{"a", "b"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "A", "b": "B"}, m)
		assert.Zero(t, s.src.calls.Load())

		require.NoError(t, s.c.DelMany(ctx, []string{"a", "b"}))
		w = s.writes()
		assert.ElementsMatch(t, []string{"l2 del a", "l2 del b"}, w[:2])
		assert.ElementsMatch(t, []string{"l1 del a", "l1 del b"}, w[2:])
		assert.Zero(t, s.m1.Len()+s.m2.Len())
	})

	t.Run("a failed layer fails every key and invalidates above", func(t *testing.T) {
		s := newTwoLayers(t)
		_, err := s.c.GetMany(ctx, []string{"a", "b"})
		require.NoError(t, err)
		s.l2.failOn("set", errBoom)
		err = s.c.SetMany(ctx, map[string]string{"a": "A", "b": "B"})
		var be *cachex.BatchError
		require.ErrorAs(t, err, &be)
		assert.ErrorIs(t, be.Errors["a"], errBoom)
		assert.ErrorIs(t, be.Errors["b"], errBoom)
		assert.Zero(t, s.m1.Len(), "the layer above is invalidated")
	})

	t.Run("keys a layer reports failed are invalidated, the others written", func(t *testing.T) {
		src := newSource(nil)
		m1 := cachextest.NewMap[string]()
		l2 := &partial{Backend: cachextest.NewMap[string](), bad: "b"}
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(m1, cachex.TTL(time.Minute, 0)),
			cachex.NewLayer(l2, cachex.TTL(time.Minute, 0)),
		})
		require.NoError(t, m1.Set(ctx, "b", cachex.Entry[string]{Value: "old", ExpiresAt: time.Now().Add(time.Hour), FreshUntil: time.Now().Add(time.Hour)}))
		err := c.SetMany(ctx, map[string]string{"a": "A", "b": "B"})
		var be *cachex.BatchError
		require.ErrorAs(t, err, &be)
		assert.Len(t, be.Errors, 1)
		assert.ErrorIs(t, be.Errors["b"], errBoom)
		e, ok := stored(t, m1, "a")
		require.True(t, ok)
		assert.Equal(t, "A", e.Value)
		_, ok = stored(t, m1, "b")
		assert.False(t, ok)
	})

	t.Run("overlapping batch writes do not deadlock", func(t *testing.T) {
		c := cachex.New(newSource(nil), oneLayer(cachextest.NewMap[string]()))
		keys := make([]string, 2000)
		for i := range keys {
			keys[i] = fmt.Sprintf("k%d", i)
		}
		var wg sync.WaitGroup
		for w := range 8 {
			wg.Go(func() {
				r := rand.New(rand.NewPCG(uint64(w), 1))
				for range 20 {
					perm := r.Perm(len(keys))[:500]
					values := map[string]string{}
					var del []string
					for j, i := range perm {
						if j%2 == 0 {
							values[keys[i]] = "v"
						} else {
							del = append(del, keys[i])
						}
					}
					assert.NoError(t, c.SetMany(ctx, values))
					assert.NoError(t, c.DelMany(ctx, del))
				}
			})
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("batch writes deadlocked")
		}
	})
}

// partial wraps a backend: SetMany fails key bad and writes the rest.
type partial struct {
	cachex.Backend[string]
	bad string
}

func (p *partial) SetMany(ctx context.Context, entries map[string]cachex.Entry[string]) error {
	rest := map[string]cachex.Entry[string]{}
	failed := map[string]error{}
	for k, e := range entries {
		if k == p.bad {
			failed[k] = errBoom
		} else {
			rest[k] = e
		}
	}
	if err := p.Backend.SetMany(ctx, rest); err != nil {
		return err
	}
	return &cachex.BatchError{Errors: failed}
}

// TestWritesAreNeverUndoneByReads interleaves writes (each changing the source
// first, then the Cache, under a per-key lock as a database row lock would) with
// reads at random. Once everything settles, every layer must agree with the
// source: no read ever backfilled a value older than a completed write.
func TestWritesAreNeverUndoneByReads(t *testing.T) {
	ctx := context.Background()
	for round := range 20 {
		src := newSource(nil)
		r := rand.New(rand.NewPCG(uint64(round), 7))
		delays := func(context.Context, string) {
			if r := rand.IntN(4); r == 0 {
				time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond)
			}
		}
		src.onGet = delays
		m1, m2 := cachextest.NewMap[string](), cachextest.NewMap[string]()
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(&jittery{m1}, cachex.TTL(time.Hour, 0), cachex.NotFoundTTL(time.Hour, 0)),
			cachex.NewLayer(&jittery{m2}, cachex.TTL(time.Hour, 0), cachex.NotFoundTTL(time.Hour, 0)),
		}, cachex.WithFetchesPerKey(1+r.IntN(3)), cachex.WithGetManyChunkSize(r.IntN(3)))
		keys := []string{"a", "b", "c"}
		var rowLocks [3]sync.Mutex
		var wg sync.WaitGroup
		for w := range 6 {
			wg.Go(func() {
				r := rand.New(rand.NewPCG(uint64(round), uint64(w)))
				for op := range 60 {
					k := r.IntN(len(keys))
					key := keys[k]
					switch r.IntN(5) {
					case 0:
						rowLocks[k].Lock()
						v := fmt.Sprintf("w%d-%d", w, op)
						src.set(key, v)
						assert.NoError(t, c.Set(ctx, key, v))
						rowLocks[k].Unlock()
					case 1:
						rowLocks[k].Lock()
						src.del(key)
						assert.NoError(t, c.Del(ctx, key))
						rowLocks[k].Unlock()
					case 2:
						rowLocks[k].Lock()
						v := fmt.Sprintf("m%d-%d", w, op)
						src.set(key, v)
						assert.NoError(t, c.SetMany(ctx, map[string]string{key: v}))
						rowLocks[k].Unlock()
					case 3:
						_, _ = c.GetMany(ctx, keys)
					default:
						_, _ = c.Get(ctx, key)
					}
				}
			})
		}
		wg.Wait()
		require.NoError(t, c.Close())
		for _, key := range keys {
			want, inSource := src.data[key]
			for i, m := range []*cachextest.Map[string]{m1, m2} {
				e, ok := stored(t, m, key)
				if !ok {
					continue
				}
				if inSource {
					assert.Equal(t, want, e.Value, "round %d layer %d key %s", round, i, key)
					assert.False(t, e.NotFound, "round %d layer %d key %s", round, i, key)
				} else {
					assert.True(t, e.NotFound, "round %d layer %d key %s holds %q", round, i, key, e.Value)
				}
			}
		}
	}
}

// jittery wraps a backend and sometimes yields before reads and writes, to
// shake out interleavings.
type jittery struct{ cachex.Backend[string] }

func yield() {
	if rand.IntN(3) == 0 {
		time.Sleep(time.Duration(rand.IntN(100)) * time.Microsecond)
	}
}

func (j *jittery) Get(ctx context.Context, key string) (cachex.Entry[string], bool, error) {
	yield()
	return j.Backend.Get(ctx, key)
}

func (j *jittery) GetMany(ctx context.Context, keys []string) (map[string]cachex.Entry[string], error) {
	yield()
	return j.Backend.GetMany(ctx, keys)
}

func (j *jittery) SetMany(ctx context.Context, entries map[string]cachex.Entry[string]) error {
	yield()
	return j.Backend.SetMany(ctx, entries)
}

func (j *jittery) Set(ctx context.Context, key string, e cachex.Entry[string]) error {
	yield()
	return j.Backend.Set(ctx, key, e)
}
