//go:build bench

// Measures what the double-check saves and costs, under open-loop load (new
// requests arrive at their own pace, as real traffic does). See
// docs/research/2026-10-double-check.md.
//
//	go test -tags bench -run TestDoubleCheckValue -v ./tools/bench/2026-10-double-check/
package bench

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/theplant/cachex/v2"
)

// slowBackend answers like a remote cache: it reads, then the reply takes
// delay to come back (the window the double-check is about).
type slowBackend struct {
	*cachex.SyncMap[*cachex.Entry[string]]
	delay time.Duration
	reads atomic.Int64
}

func (s *slowBackend) Get(ctx context.Context, key string) (*cachex.Entry[string], error) {
	s.reads.Add(1)
	v, err := s.SyncMap.Get(ctx, key)
	time.Sleep(s.delay)
	return v, err
}

// run sends 20 requests per ms for 400 ms; hot uses one key that expires
// every 20 ms, otherwise every request asks for a new (cold) key.
func run(mode cachex.DoubleCheckMode, delay time.Duration, hot bool) (upstreamCalls int64, readsPerGet float64) {
	ctx := context.Background()
	backend := &slowBackend{SyncMap: cachex.NewSyncMap[*cachex.Entry[string]](), delay: delay}
	var calls atomic.Int64
	up := cachex.UpstreamFunc[*cachex.Entry[string]](func(context.Context, string) (*cachex.Entry[string], error) {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		return &cachex.Entry[string]{Data: "v", CachedAt: cachex.NowFunc()}, nil
	})
	cli := cachex.NewClient[*cachex.Entry[string]](backend, up,
		cachex.EntryWithTTL[string](20*time.Millisecond, 0),
		cachex.WithDoubleCheck[*cachex.Entry[string]](mode))

	var n atomic.Int64
	var wg sync.WaitGroup
	for ms := range 400 {
		for j := range 20 {
			key := "hot"
			if !hot {
				key = fmt.Sprintf("cold-%d-%d", ms, j)
			}
			wg.Go(func() {
				_, _ = cli.Get(ctx, key)
				n.Add(1)
			})
		}
		time.Sleep(time.Millisecond)
	}
	wg.Wait()
	return calls.Load(), float64(backend.reads.Load()) / float64(n.Load())
}

// spread is the min, median and max of xs.
func spread[N int64 | float64](xs []N) (lo, mid, hi N) {
	xs = slices.Clone(xs)
	slices.Sort(xs)
	return xs[0], xs[len(xs)/2], xs[len(xs)-1]
}

func TestDoubleCheckValue(t *testing.T) {
	const rounds = 5
	modes := []struct {
		name string
		mode cachex.DoubleCheckMode
	}{{"enabled", cachex.DoubleCheckEnabled}, {"auto", cachex.DoubleCheckAuto}, {"disabled", cachex.DoubleCheckDisabled}}
	// medians by "hot|cold/delay/mode", for the assertions below
	callsMid, readsMid := map[string]int64{}, map[string]float64{}
	for _, hot := range []bool{true, false} {
		for _, delay := range []time.Duration{0, 200 * time.Microsecond, time.Millisecond} {
			if !hot && delay != time.Millisecond {
				continue // cold keys: only the cost matters, one latency is enough
			}
			for _, m := range modes {
				calls, reads := make([]int64, rounds), make([]float64, rounds)
				for i := range rounds {
					calls[i], reads[i] = run(m.mode, delay, hot)
				}
				scenario := "hot key, expires every 20ms"
				id := fmt.Sprintf("hot/%v/%s", delay, m.name)
				if !hot {
					scenario = "8000 cold keys"
					id = fmt.Sprintf("cold/%v/%s", delay, m.name)
				}
				cLo, cMid, cHi := spread(calls)
				rLo, rMid, rHi := spread(reads)
				callsMid[id], readsMid[id] = cMid, rMid
				t.Logf("%-28s backend=%-6v %-9s upstream calls %3d [%d..%d]   backend reads per Get %.3f [%.3f..%.3f]",
					scenario, delay, m.name, cMid, cLo, cHi, rMid, rLo, rHi)
			}
		}
	}

	// hot key behind a 1ms backend: the double-check saves upstream calls,
	// and the on-demand one keeps most of the saving
	assert.Greater(t, float64(callsMid["hot/1ms/disabled"]), 1.2*float64(callsMid["hot/1ms/enabled"]))
	assert.Less(t, callsMid["hot/1ms/auto"], callsMid["hot/1ms/disabled"])
	// cold keys: always on reads the backend twice per miss, on demand almost never
	assert.Greater(t, readsMid["cold/1ms/enabled"], 1.9)
	assert.Less(t, readsMid["cold/1ms/auto"], 1.05)
	assert.InDelta(t, 1.0, readsMid["cold/1ms/disabled"], 0.001)
}
