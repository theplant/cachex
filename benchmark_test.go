package cachex

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

	"golang.org/x/sync/singleflight"
)

// Micro benchmarks of cachex's own cost: every upstream answers at once, so
// the numbers are the library's overhead per call, not simulated I/O.
// Compare runs with: go test -run '^$' -bench . -benchmem -count=10 | benchstat

// benchUpstream answers every key except those in missing, at once.
func benchUpstream(calls *atomic.Int64, missing func(string) bool) batchUpstreamFunc[string] {
	return func(_ context.Context, keys []string) (map[string]string, error) {
		if calls != nil {
			calls.Add(1)
		}
		out := make(map[string]string, len(keys))
		for _, k := range keys {
			if missing == nil || !missing(k) {
				out[k] = "v"
			}
		}
		return out, nil
	}
}

func benchKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "key-" + strconv.Itoa(i)
	}
	return keys
}

// waitRefreshes waits for c's background refreshes, so none outlives the benchmark.
func waitRefreshes(c *Client[*Entry[string]]) {
	for {
		busy := false
		c.asyncRefreshing.Range(func(any, any) bool { busy = true; return false })
		if !busy {
			return
		}
		runtime.Gosched()
	}
}

func BenchmarkGet(b *testing.B) {
	ctx := context.Background()
	hit := func(b *testing.B, c *Client[string]) {
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

	b.Run("hit/syncmap", func(b *testing.B) {
		hit(b, NewClient(NewSyncMap[string](), benchUpstream(nil, nil)))
	})
	b.Run("hit/ristretto", func(b *testing.B) {
		r, err := NewRistrettoCache(DefaultRistrettoCacheConfig[string]())
		if err != nil {
			b.Fatal(err)
		}
		defer r.Close()
		hit(b, NewClient(r, benchUpstream(nil, nil)))
	})
	b.Run("hit/l2", func(b *testing.B) {
		// L1 misses every time and finds the key in L2, then backfills L1
		l1Backend := NewSyncMap[string]()
		l2 := NewClient(NewSyncMap[string](), benchUpstream(nil, nil))
		l1 := NewClient(l1Backend, l2)
		if _, err := l1.Get(ctx, "k"); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			_ = l1Backend.Del(ctx, "k")
			if _, err := l1.Get(ctx, "k"); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("notfound-hit", func(b *testing.B) {
		c := NewClient(NewSyncMap[string](), benchUpstream(nil, func(string) bool { return true }),
			NotFoundWithTTL[string](NewSyncMap[time.Time](), time.Hour, 0))
		_, _ = c.Get(ctx, "k") // records the not-found
		b.ReportAllocs()
		for b.Loop() {
			if _, err := c.Get(ctx, "k"); !IsErrKeyNotFound(err) {
				b.Fatal(err)
			}
		}
	})

	b.Run("stale-hit", func(b *testing.B) {
		// the upstream keeps answering an old entry, so the key stays stale
		// and every read that finds no refresh running starts one
		old := &Entry[string]{Data: "v", CachedAt: time.Now().Add(-time.Minute)}
		backend := NewSyncMap[*Entry[string]]()
		_ = backend.Set(ctx, "k", old)
		c := NewClient(backend, UpstreamFunc[*Entry[string]](func(context.Context, string) (*Entry[string], error) {
			return old, nil
		}), EntryWithTTL[string](time.Second, time.Hour), WithServeStale[*Entry[string]](true))
		defer waitRefreshes(c)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := c.Get(ctx, "k"); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("miss", func(b *testing.B) {
		backend := NewSyncMap[string]()
		c := NewClient(backend, benchUpstream(nil, nil))
		b.ReportAllocs()
		for b.Loop() {
			_ = backend.Del(ctx, "k")
			if _, err := c.Get(ctx, "k"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("miss/x-sync-baseline", func(b *testing.B) {
		// the least a deduplicating read-through can do: a map and x/sync's singleflight
		var m sync.Map
		var g singleflight.Group
		up := benchUpstream(nil, nil)
		get := func(key string) (string, error) {
			if v, ok := m.Load(key); ok {
				return v.(string), nil
			}
			v, err, _ := g.Do(key, func() (any, error) {
				v, err := up.Get(ctx, key)
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
			// half-miss: odd keys do not exist upstream and nothing records
			// that, so they miss and reach the upstream on every call
			var missing func(string) bool
			if mode == "half-miss" {
				missing = func(k string) bool { return k[len(k)-1]%2 == 1 }
			}
			c := NewClient(NewSyncMap[string](), benchUpstream(nil, missing))
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
						if _, err := c.Get(ctx, k); err != nil && !IsErrKeyNotFound(err) {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}

// BenchmarkHotKeyStampede releases many concurrent misses of one key at once;
// the upstream must be called exactly once per round.
func BenchmarkHotKeyStampede(b *testing.B) {
	const n = 64
	ctx := context.Background()
	backend := NewSyncMap[string]()
	var calls, entered atomic.Int64
	up := UpstreamFunc[string](func(context.Context, string) (string, error) {
		calls.Add(1)
		for entered.Load() < n { // hold the fetch until every reader is in
			runtime.Gosched()
		}
		return "v", nil
	})
	c := NewClient(backend, up)
	b.ReportAllocs()
	for b.Loop() {
		_ = backend.Del(ctx, "k")
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
		b.Fatalf("upstream calls per stampede = %v, want 1", got)
	}
	b.ReportMetric(float64(calls.Load())/float64(b.N), "upstream-calls/op")
}

// BenchmarkSetDel writes in parallel while a reader keeps reading the same
// keys: "same-stripe" puts every key in one stripe, "spread" spreads them.
func BenchmarkSetDel(b *testing.B) {
	ctx := context.Background()
	c := NewClient(NewSyncMap[string](), benchUpstream(nil, nil))
	spread := benchKeys(1024)
	var same []string
	for _, k := range benchKeys(1 << 20) {
		if c.stripe(k) == c.stripe(spread[0]) {
			same = append(same, k)
			if len(same) == len(spread) {
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
// keys; every 10th key does not exist upstream (and no not-found cache), so
// those reads miss each time.
func BenchmarkZipfMixed(b *testing.B) {
	const n = 10000
	ctx := context.Background()
	keys := benchKeys(n)
	missing := map[string]bool{}
	for i := 0; i < n; i += 10 {
		missing[keys[i]] = true
	}
	c := NewClient(NewSyncMap[string](), benchUpstream(nil, func(k string) bool { return missing[k] }))
	if _, err := c.GetMany(ctx, keys); err != nil {
		b.Fatal(err)
	}
	var seed atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		z := rand.NewZipf(rand.New(rand.NewPCG(seed.Add(1), 0)), 1.1, 1, n-1)
		for pb.Next() {
			if _, err := c.Get(ctx, keys[z.Uint64()]); err != nil && !IsErrKeyNotFound(err) {
				b.Error(err)
				return
			}
		}
	})
}
