package cachex_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/allegro/bigcache/v3"
	"golang.org/x/sync/singleflight"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/bigcachex"
	"github.com/theplant/cachex/v2/cachextest"
	"github.com/theplant/cachex/v2/ottercachex"
)

// Micro benchmarks of cachex's own cost: every source answers at once, so the
// numbers are the library's overhead per call, not simulated I/O.
// Compare runs with: go test -run '^$' -bench . -benchmem -count=10 | benchstat

// benchSource answers every key except those missing, at once.
type benchSource struct{ missing func(string) bool }

func (s benchSource) Get(_ context.Context, key string) (string, error) {
	if s.missing != nil && s.missing(key) {
		return "", cachex.ErrNotFound
	}
	return "v", nil
}

func (s benchSource) GetMany(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if s.missing == nil || !s.missing(k) {
			out[k] = "v"
		}
	}
	return out, nil
}

func benchKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "key-" + strconv.Itoa(i)
	}
	return keys
}

func layer(b cachex.Backend[string], opts ...cachex.LayerOption) cachex.Layer[string] {
	return cachex.NewLayer(b, append([]cachex.LayerOption{cachex.TTL(time.Hour, 0)}, opts...)...)
}

func BenchmarkGet(b *testing.B) {
	ctx := context.Background()
	hit := func(b *testing.B, c *cachex.Cache[string]) {
		b.Helper()
		if _, err := c.Get(ctx, "k"); err != nil { // warm
			b.Fatal(err)
		}
		b.Run("serial", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := c.Get(ctx, "k"); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("parallel", func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := c.Get(ctx, "k"); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}

	b.Run("hit/map", func(b *testing.B) {
		hit(b, cachex.New[string](benchSource{}, []cachex.Layer[string]{layer(cachextest.NewMap[string]())}))
	})
	b.Run("hit/otter", func(b *testing.B) {
		o, err := ottercachex.New[string](ottercachex.Config[string]{MaximumSize: 10_000})
		if err != nil {
			b.Fatal(err)
		}
		hit(b, cachex.New[string](benchSource{}, []cachex.Layer[string]{layer(o)}))
	})
	b.Run("hit/bigcache", func(b *testing.B) {
		bc, err := bigcache.New(ctx, bigcache.DefaultConfig(time.Hour))
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = bc.Close() }()
		hit(b, cachex.New[string](benchSource{}, []cachex.Layer[string]{layer(bigcachex.New[string](bigcachex.Config[string]{Cache: bc}))}))
	})
	b.Run("hit/l2", func(b *testing.B) {
		// the first layer misses every time and finds the key in the second,
		// then backfills the first
		l1 := cachextest.NewMap[string]()
		c := cachex.New[string](benchSource{}, []cachex.Layer[string]{layer(l1), layer(cachextest.NewMap[string]())})
		if _, err := c.Get(ctx, "k"); err != nil {
			b.Fatal(err)
		}
		// the read and its backfill, which finishes after the read returns:
		// each round waits for it, or the next read would join this one
		b.ReportAllocs()
		for b.Loop() {
			_ = l1.Del(ctx, "k")
			if _, err := c.Get(ctx, "k"); err != nil {
				b.Fatal(err)
			}
			cachex.Settle(c)
		}
	})

	b.Run("notfound-hit", func(b *testing.B) {
		c := cachex.New[string](benchSource{missing: func(string) bool { return true }},
			[]cachex.Layer[string]{layer(cachextest.NewMap[string](), cachex.NotFoundTTL(time.Hour, 0))})
		_, _ = c.Get(ctx, "k") // records the not-found
		b.ReportAllocs()
		for b.Loop() {
			if _, err := c.Get(ctx, "k"); err != cachex.ErrNotFound { //nolint:errorlint // the sentinel itself
				b.Fatal(err)
			}
		}
	})

	b.Run("stale-hit", func(b *testing.B) {
		// the clock moves a minute per reading, so every entry is stale by the
		// next read and every read that finds no refresh running starts one
		var tick atomic.Int64
		now := func() time.Time { return epoch.Add(time.Duration(tick.Add(1)) * time.Minute) }
		c := cachex.New[string](benchSource{}, []cachex.Layer[string]{
			cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Second, 24*time.Hour)),
		}, cachex.WithNow(now))
		if _, err := c.Get(ctx, "k"); err != nil {
			b.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		b.ReportAllocs()
		for b.Loop() {
			if _, err := c.Get(ctx, "k"); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("miss", func(b *testing.B) {
		mem := cachextest.NewMap[string]()
		c := cachex.New[string](benchSource{}, []cachex.Layer[string]{layer(mem)})
		// the read and its backfill, which finishes after the read returns:
		// each round waits for it, or the next read would join this one
		b.ReportAllocs()
		for b.Loop() {
			_ = mem.Del(ctx, "k")
			if _, err := c.Get(ctx, "k"); err != nil {
				b.Fatal(err)
			}
			cachex.Settle(c)
		}
	})
	b.Run("miss/x-sync-baseline", func(b *testing.B) {
		// the least a deduplicating read-through can do: a map and x/sync's singleflight
		var m sync.Map
		var g singleflight.Group
		src := benchSource{}
		get := func(key string) (string, error) {
			if v, ok := m.Load(key); ok {
				return v.(string), nil
			}
			v, err, _ := g.Do(key, func() (any, error) {
				v, err := src.Get(ctx, key)
				if err == nil {
					m.Store(key, v)
				}
				return v, err
			})
			return v.(string), err
		}
		b.ReportAllocs()
		for b.Loop() {
			m.Delete("k")
			if _, err := get("k"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkGetMany(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{10, 100, 1000} {
		keys := benchKeys(n)
		for _, mode := range []string{"hit", "half-miss"} {
			// half-miss: odd keys do not exist and nothing records that, so
			// they reach the source on every call
			var missing func(string) bool
			if mode == "half-miss" {
				missing = func(k string) bool { return k[len(k)-1]%2 == 1 }
			}
			c := cachex.New[string](benchSource{missing: missing}, []cachex.Layer[string]{layer(cachextest.NewMap[string]())})
			if _, err := c.GetMany(ctx, keys); err != nil {
				b.Fatal(err)
			}
			b.Run(fmt.Sprintf("%s/n=%d/GetMany", mode, n), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := c.GetMany(ctx, keys); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(fmt.Sprintf("%s/n=%d/loop-Get", mode, n), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					for _, k := range keys {
						if _, err := c.Get(ctx, k); err != nil && err != cachex.ErrNotFound { //nolint:errorlint // the sentinel itself
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}

// BenchmarkHotKeyStampede releases many concurrent misses of one key at once;
// the source must be called exactly once per round.
func BenchmarkHotKeyStampede(b *testing.B) {
	const n = 64
	ctx := context.Background()
	mem := cachextest.NewMap[string]()
	var calls, entered atomic.Int64
	src := cachex.SourceFunc[string](func(context.Context, string) (string, error) {
		calls.Add(1)
		for entered.Load() < n { // hold the fetch until every reader is in
			runtime.Gosched()
		}
		return "v", nil
	})
	c := cachex.New(src, []cachex.Layer[string]{layer(mem)})
	b.ReportAllocs()
	for b.Loop() {
		_ = mem.Del(ctx, "k")
		entered.Store(0)
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				entered.Add(1)
				if _, err := c.Get(ctx, "k"); err != nil {
					b.Error(err)
				}
			})
		}
		wg.Wait()
	}
	if got := float64(calls.Load()) / float64(b.N); got != 1 {
		b.Fatalf("source calls per stampede = %v, want 1", got)
	}
	b.ReportMetric(float64(calls.Load())/float64(b.N), "source-calls/op")
}

// BenchmarkSetDel writes in parallel while a reader keeps reading the same
// keys: "same-stripe" puts every key in one stripe, "spread" spreads them.
func BenchmarkSetDel(b *testing.B) {
	ctx := context.Background()
	c := cachex.New[string](benchSource{}, []cachex.Layer[string]{layer(cachextest.NewMap[string]())})
	spread := benchKeys(1024)
	var same []string
	for _, k := range benchKeys(1 << 22) {
		if cachex.StripeIndex(c, k) == cachex.StripeIndex(c, spread[0]) {
			if same = append(same, k); len(same) == len(spread) {
				break
			}
		}
	}
	for name, keys := range map[string][]string{"same-stripe": same, "spread": spread} {
		b.Run(name, func(b *testing.B) {
			stop := make(chan struct{})
			var readers sync.WaitGroup
			readers.Go(func() {
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
						_, _ = c.Get(ctx, keys[i%len(keys)])
					}
				}
			})
			defer func() { close(stop); readers.Wait() }()
			var next atomic.Int64
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					i := next.Add(1)
					k := keys[i%int64(len(keys))]
					var err error
					if i%2 == 0 {
						err = c.Set(ctx, k, "v")
					} else {
						err = c.Del(ctx, k)
					}
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

// BenchmarkZipfMixed reads keys drawn from a Zipf distribution over 10,000
// keys from many goroutines; every 10th key does not exist (and no not-found
// TTL records that), so those reads miss each time.
func BenchmarkZipfMixed(b *testing.B) {
	const n = 10000
	ctx := context.Background()
	keys := benchKeys(n)
	missing := map[string]bool{}
	for i := 0; i < n; i += 10 {
		missing[keys[i]] = true
	}
	o, err := ottercachex.New[string](ottercachex.Config[string]{MaximumSize: 2 * n})
	if err != nil {
		b.Fatal(err)
	}
	for name, backend := range map[string]cachex.Backend[string]{"map": cachextest.NewMap[string](), "otter": o} {
		b.Run(name, func(b *testing.B) {
			c := cachex.New[string](benchSource{missing: func(k string) bool { return missing[k] }}, []cachex.Layer[string]{layer(backend)})
			if _, err := c.GetMany(ctx, keys); err != nil {
				b.Fatal(err)
			}
			var seed atomic.Uint64
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				z := rand.NewZipf(rand.New(rand.NewPCG(seed.Add(1), 0)), 1.1, 1, n-1)
				for pb.Next() {
					if _, err := c.Get(ctx, keys[z.Uint64()]); err != nil && err != cachex.ErrNotFound { //nolint:errorlint // the sentinel itself
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
