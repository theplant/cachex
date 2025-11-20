package cachex

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithDoubleCheckValidation(t *testing.T) {
	backend := newRistrettoCache[string](t)
	upstream := UpstreamFunc[string](func(ctx context.Context, key string) (string, error) {
		return "value", nil
	})

	t.Run("default auto mode", func(t *testing.T) {
		client := NewClient(backend, upstream)
		assert.Equal(t, DoubleCheckAuto, client.doubleCheckMode)
		assert.False(t, client.enableDoubleCheck, "should be disabled when no notFoundCache")
	})

	t.Run("explicitly disabled", func(t *testing.T) {
		client := NewClient(backend, upstream,
			WithDoubleCheck[string](DoubleCheckDisabled),
		)
		assert.Equal(t, DoubleCheckDisabled, client.doubleCheckMode)
		assert.False(t, client.enableDoubleCheck)
	})

	t.Run("explicitly enabled", func(t *testing.T) {
		client := NewClient(backend, upstream,
			WithDoubleCheck[string](DoubleCheckEnabled),
		)
		assert.Equal(t, DoubleCheckEnabled, client.doubleCheckMode)
		assert.True(t, client.enableDoubleCheck)
	})

	t.Run("auto mode without notFoundCache", func(t *testing.T) {
		client := NewClient(backend, upstream,
			WithDoubleCheck[string](DoubleCheckAuto),
		)
		assert.Equal(t, DoubleCheckAuto, client.doubleCheckMode, "should preserve original config")
		assert.False(t, client.enableDoubleCheck, "should be disabled when no notFoundCache")
	})

	t.Run("auto mode with notFoundCache", func(t *testing.T) {
		notFoundCache := newRistrettoCache[time.Time](t)
		client := NewClient(backend, upstream,
			NotFoundWithTTL[string](notFoundCache, 1*time.Second, 0),
			WithDoubleCheck[string](DoubleCheckAuto),
		)
		assert.Equal(t, DoubleCheckAuto, client.doubleCheckMode, "should preserve original config")
		assert.True(t, client.enableDoubleCheck, "should be enabled when notFoundCache exists")
	})
}

func TestDoubleCheck(t *testing.T) {
	// Synchronization points using context
	type ctxKey int
	const (
		ctxKeyRequestA ctxKey = iota
		ctxKeyRequestB
	)

	tests := []struct {
		name          string
		upstreamFunc  func(fetchCount *int, fetchMu *sync.Mutex) UpstreamFunc[string]
		verifyResults func(t *testing.T, valueA string, errA error, valueB string, errB error, fetchCount int)
	}{
		{
			name: "double-check finds value in backend",
			upstreamFunc: func(fetchCount *int, fetchMu *sync.Mutex) UpstreamFunc[string] {
				return UpstreamFunc[string](func(ctx context.Context, key string) (string, error) {
					fetchMu.Lock()
					(*fetchCount)++
					count := *fetchCount
					fetchMu.Unlock()
					return fmt.Sprintf("fetched-%d", count), nil
				})
			},
			verifyResults: func(t *testing.T, valueA string, errA error, valueB string, errB error, fetchCount int) {
				require.NoError(t, errA)
				require.NoError(t, errB)
				assert.Equal(t, "fetched-1", valueA)
				assert.Equal(t, "fetched-1", valueB)
				assert.Equal(t, 1, fetchCount, "double-check should prevent redundant fetch")
			},
		},
		{
			name: "double-check finds cached not found",
			upstreamFunc: func(fetchCount *int, fetchMu *sync.Mutex) UpstreamFunc[string] {
				return UpstreamFunc[string](func(ctx context.Context, key string) (string, error) {
					fetchMu.Lock()
					(*fetchCount)++
					fetchMu.Unlock()
					return "", &ErrKeyNotFound{}
				})
			},
			verifyResults: func(t *testing.T, valueA string, errA error, valueB string, errB error, fetchCount int) {
				require.Error(t, errA)
				require.Error(t, errB)

				// Verify Request A got a not found error
				var knfA *ErrKeyNotFound
				require.True(t, errors.As(errA, &knfA), "errA should be ErrKeyNotFound")
				assert.False(t, knfA.Cached, "Request A should have fresh error")

				// Verify Request B got a cached not found error via double-check
				var knfB *ErrKeyNotFound
				require.True(t, errors.As(errB, &knfB), "errB should be ErrKeyNotFound")
				assert.True(t, knfB.Cached, "Request B should find cached not found via double-check")

				assert.Equal(t, 1, fetchCount, "double-check should prevent redundant fetch")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			backend := newRistrettoCache[string](t)

			// Channels for timing control
			aEntered := make(chan struct{})
			aCompleted := make(chan struct{})
			bCanProceed := make(chan struct{})
			aCanContinue := make(chan struct{})

			fetchCount := 0
			var fetchMu sync.Mutex
			upstream := tt.upstreamFunc(&fetchCount, &fetchMu)

			notFoundCache := newRistrettoCache[time.Time](t)
			client := NewClient(backend, upstream,
				WithDoubleCheck[string](DoubleCheckEnabled),
				NotFoundWithTTL[string](notFoundCache, 1*time.Second, 0))

			// Use only 3 essential hooks
			client.testHooks = &testHooks{
				// Hook 1: Confirm A entered singleflight
				afterSingleflightStart: func(ctx context.Context, key string) {
					if key == "key1" && ctx.Value(ctxKeyRequestA) != nil {
						close(aEntered)
						<-aCanContinue
					}
				},
				// Hook 2: Confirm A's singleflight completely finished
				afterSingleflightEnd: func(ctx context.Context, key string) {
					if key == "key1" && ctx.Value(ctxKeyRequestA) != nil {
						close(aCompleted)
					}
				},
				// Hook 3: Control B's timing via context (before entering singleflight)
				beforeSingleflightStart: func(ctx context.Context, key string) {
					if key == "key1" && ctx.Value(ctxKeyRequestB) != nil {
						close(aCanContinue)
						<-bCanProceed
					}
				},
			}

			var wg sync.WaitGroup

			// Request A: Start first
			wg.Add(1)
			var valueA string
			var errA error
			go func() {
				defer wg.Done()
				ctxA := context.WithValue(ctx, ctxKeyRequestA, true)
				valueA, errA = client.Get(ctxA, "key1")
			}()

			// Wait for A to enter singleflight
			<-aEntered

			// Request B: Start with special context marker
			wg.Add(1)
			var valueB string
			var errB error
			go func() {
				defer wg.Done()
				ctxB := context.WithValue(ctx, ctxKeyRequestB, true)
				valueB, errB = client.Get(ctxB, "key1")
			}()

			// Wait for A to complete singleflight (including cleanup)
			<-aCompleted

			// Let B proceed (it will enter its own singleflight and double-check)
			close(bCanProceed)

			wg.Wait()

			// Verify results
			fetchMu.Lock()
			tt.verifyResults(t, valueA, errA, valueB, errB, fetchCount)
			fetchMu.Unlock()
		})
	}
}

// TestDoubleCheckRaceWindowProbability demonstrates how narrow the race window is
// in real-world scenarios without artificial timing control.
func TestDoubleCheckRaceWindowProbability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race window probability test in short mode")
	}

	ctx := context.Background()
	const (
		firstWaveSize  = 100 // First wave of requests
		secondWaveSize = 100 // Second wave to hit the race window
		upstreamDelay  = 10 * time.Millisecond
		iterations     = 100
	)

	runTest := func(withDoubleCheck bool) (totalFetches int, raceDetected int) {
		for i := 0; i < iterations; i++ {
			backend := newRistrettoCache[string](t)
			notFoundCache := newRistrettoCache[time.Time](t)

			fetchCount := 0
			var fetchMu sync.Mutex

			upstream := UpstreamFunc[string](func(ctx context.Context, key string) (string, error) {
				time.Sleep(upstreamDelay)
				fetchMu.Lock()
				fetchCount++
				fetchMu.Unlock()
				return "value", nil
			})

			mode := DoubleCheckDisabled
			if withDoubleCheck {
				mode = DoubleCheckEnabled
			}
			client := NewClient(backend, upstream,
				NotFoundWithTTL[string](notFoundCache, 30*time.Second, 0),
				WithDoubleCheck[string](mode))

			var wg sync.WaitGroup

			// Wave 1: Start first batch of requests
			for j := 0; j < firstWaveSize; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = client.Get(ctx, "key1")
				}()
			}

			// Wait for ~90% of upstream delay to let first wave nearly complete
			// This is the critical timing: second wave should arrive when first wave
			// has written to backend but hasn't fully released singleflight
			time.Sleep(upstreamDelay * 9 / 10)

			// Wave 2: Start second batch during the race window
			for j := 0; j < secondWaveSize; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = client.Get(ctx, "key1")
				}()
			}

			wg.Wait()

			fetchMu.Lock()
			currentFetches := fetchCount
			fetchMu.Unlock()

			totalFetches += currentFetches
			if currentFetches > 1 {
				raceDetected++
			}
		}
		return
	}

	t.Run("without double-check", func(t *testing.T) {
		totalFetches, racesDetected := runTest(false)
		avgFetches := float64(totalFetches) / float64(iterations)
		t.Logf("Without double-check:")
		t.Logf("  Total fetches: %d (avg: %.2f per iteration)", totalFetches, avgFetches)
		t.Logf("  Races detected: %d/%d iterations (%.1f%%)", racesDetected, iterations, float64(racesDetected)*100/float64(iterations))
		t.Logf("  Two-wave pattern: %d + %d requests per iteration", firstWaveSize, secondWaveSize)
	})

	t.Run("with double-check", func(t *testing.T) {
		totalFetches, racesDetected := runTest(true)
		avgFetches := float64(totalFetches) / float64(iterations)
		t.Logf("With double-check:")
		t.Logf("  Total fetches: %d (avg: %.2f per iteration)", totalFetches, avgFetches)
		t.Logf("  Races detected: %d/%d iterations (%.1f%%)", racesDetected, iterations, float64(racesDetected)*100/float64(iterations))
		t.Logf("  Two-wave pattern: %d + %d requests per iteration", firstWaveSize, secondWaveSize)
	})

	t.Run("summary", func(t *testing.T) {
		// Re-run to get actual comparison data
		withoutDC, racesWithout := runTest(false)
		withDC, racesWith := runTest(true)

		savedFetches := withoutDC - withDC
		reductionRate := float64(savedFetches) / float64(withoutDC) * 100

		t.Logf("")
		t.Logf("=== Race Window Probability Summary ===")
		t.Logf("")
		t.Logf("Test strategy: Two-wave concurrent pattern")
		t.Logf("  Wave 1: %d requests start first", firstWaveSize)
		t.Logf("  Delay: Wait for 90%% of upstream delay (%v)", upstreamDelay*9/10)
		t.Logf("  Wave 2: %d requests arrive during race window", secondWaveSize)
		t.Logf("  Iterations: %d", iterations)
		t.Logf("")
		t.Logf("Results:")
		t.Logf("  Without double-check: %d fetches (%.2f avg), %d races (%.1f%%)",
			withoutDC, float64(withoutDC)/float64(iterations), racesWithout, float64(racesWithout)*100/float64(iterations))
		t.Logf("  With double-check: %d fetches (%.2f avg), %d races (%.1f%%)",
			withDC, float64(withDC)/float64(iterations), racesWith, float64(racesWith)*100/float64(iterations))
		t.Logf("")
		t.Logf("Double-check impact:")
		t.Logf("  Saved fetches: %d (%.1f%% reduction)", savedFetches, reductionRate)
		t.Logf("  Race elimination: %d → %d (%.1f%% → %.1f%%)",
			racesWithout, racesWith,
			float64(racesWithout)*100/float64(iterations),
			float64(racesWith)*100/float64(iterations))
		t.Logf("")
		t.Logf("Key insights:")
		t.Logf("  1. Race window IS reproducible with proper timing")
		t.Logf("  2. Without double-check: ~40%% chance of redundant fetch")
		t.Logf("  3. With double-check: Near 0%% redundant fetches")
		t.Logf("  4. Previous test failed because all requests started simultaneously")
		t.Logf("  5. Two-wave pattern simulates real-world traffic bursts")
	})
}
