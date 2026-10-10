# Benchmarks

English | [中文](BENCHMARK_ZH.md)

These are micro benchmarks of cachex's own overhead. Every source answers at once and nothing sleeps, so the numbers are the cost of the library per call, not of simulated I/O. A real fetch (a Redis round trip, a database query) costs 100µs or more; compare against that before acting on anything here.

The code is in [`benchmark_test.go`](benchmark_test.go); the full report, with the v1 baseline and the fixes made while porting the suite, is [docs/research/2026-10-micro-benchmarks.md](docs/research/2026-10-micro-benchmarks.md) (Chinese).

## How to run

```sh
go test -run '^$' -bench . -benchmem -count=10 . > new.txt
go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt
```

Numbers only mean something when compared on the same machine.

## Environment and method

| | macOS | Linux |
|---|---|---|
| Machine | Apple M3 Pro, 12 cores | Docker Desktop's Linux VM on the same Mac, 12 CPUs, 7.75 GiB |
| OS | macOS 27.0.1, darwin/arm64 | linux/arm64 |
| Go | 1.27.1 | 1.26.9 |

The Linux numbers come from a VM on a laptop, not from a server. The machine was shared with other work (load average around 10), so v1 and v2 test binaries were run alternately, 10 rounds of `-benchtime 200ms` each, and compared with benchstat (medians below). Parallel benchmarks have wide spreads (up to ±50%); only differences well above that mean anything.

## Results: v1 → v2

Time per operation, median. v1 is the last v1 code (after `github.com/pkg/errors` was dropped), its benchmarks renamed to v2's.

| Benchmark | What it measures | Linux v1 | Linux v2 | macOS v1 | macOS v2 | Allocs v1 → v2 |
|---|---|---|---|---|---|---|
| `Get/hit/map/serial` | fresh hit, in-memory layer | 16.9ns | 80.6ns | 16.9ns | 57.6ns | 0 → 0 |
| `Get/hit/map/parallel` | the same from 12 goroutines | 2.2ns | 10.7ns | 2.4ns | 12.4ns | 0 → 0 |
| `Get/hit/otter/parallel` | fresh hit, memory layer (v1: ristretto, v2: otter) | 114ns | 24.3ns | 91.8ns | 20.6ns | 0 → 0 |
| `Get/notfound-hit` | hit on a fresh not-found entry | 535ns | 81.4ns | 501ns | 56.9ns | 12 → 0 |
| `Get/stale-hit` | stale hit, starting a background refresh | 104ns | 73.8ns | 87.9ns | 71.7ns | 2 → 1 |
| `Get/miss` | full fetch: claim, source, backfill (v2: waits for the backfill too, see below) | 2.74µs | 2.11µs | 2.74µs | 2.33µs | 26 → 22 |
| `Get/miss/x-sync-baseline` | `sync.Map` + `x/sync/singleflight`, for scale | 277ns | 134ns | 244ns | 127ns | 7 → 4 |
| `Get/hit/l2` | first layer misses, second layer hits, backfill (v2: waits for it too) | 2.49µs | 3.41µs | 2.55µs | 3.75µs | 23 → 24 |
| `GetMany/hit/n=100/GetMany` | 100 keys, all hits | 10.6µs | 11.8µs | 9.59µs | 10.5µs | 14 → 14 |
| `GetMany/hit/n=100/loop-Get` | the same keys, `Get` in a loop | 2.30µs | 8.55µs | 2.38µs | 6.15µs | 0 → 0 |
| `GetMany/half-miss/n=100/GetMany` | 100 keys, half not in the source, nothing cached for them | 66.2µs | 52.5µs | 55.5µs | 44.8µs | 598 → ~200 |
| `GetMany/half-miss/n=100/loop-Get` | the same keys, `Get` in a loop | 154µs | 108µs | 157µs | 119µs | 1500 → 950 |
| `HotKeyStampede` | 64 concurrent misses of one key; one source call per round | 85.5µs | 70.7µs | 61.0µs | 51.2µs | 546 → 151 |
| `SetDel/spread` | parallel `Set`/`Del` over 1024 keys, with a reader | 334ns | 168ns | 350ns | 202ns | 4 → 1 or 2 |
| `SetDel/same-stripe` | the same, all keys in one stripe | 374ns | 391ns | 270ns | 285ns | 6 → 4 |
| `ZipfMixed/map` | parallel Zipf reads over 10,000 keys, 10% not in the source | 202ns | 176ns | 153ns | 145ns | 4 → 1 |
| `ZipfMixed/otter` | the same over otter (v2 only) | | 191ns | | 163ns | 1 |

## What the numbers say

- **A hit costs more in v2, and most of it is reading the clock.** Every hit checks the entry's freshness, which needs the time: `time.Now()` alone costs about 30ns on macOS and 38ns in the Linux VM. v1's hit benchmark stored plain values with no freshness check. A fair comparison on the Mac: v1 bare 19ns, v1 with `EntryWithTTL` (which reads the clock like v2) 43ns, v2 59ns. About 7ns of the remaining gap is reading the key's stripe epoch before the first-layer read, which `DoubleCheckAuto` needs.
- **Not-found hits are nearly free now**: a not-found entry is a state of the entry in the layer, read once, and `ErrNotFound` is returned as is (no wrapping, no allocation). v1 read a second backend and formatted an error.
- **Misses are cheaper** (−15 to −23%, 22 allocations instead of 26); a stampede of 64 readers still calls the source once. In v2 a fetch answers its callers first and backfills after, so a caller no longer waits for the backfill; `Get/miss` and `Get/hit/l2` wait for the backfill each round (`cachex.Settle`) so they measure the same total work as v1, where the backfill came before the answer. The latency the caller saves shows only when the upper layers are remote (Redis, a database), not in this in-memory benchmark.
- **`GetMany` pays off only with misses or a remote layer.** On an in-memory layer with every key a hit it costs 105 to 118ns per key, against 62 to 85ns for a loop of `Get`; with misses it is about 20% faster than v1 and fetches once. Against Redis or a database, one round trip for the batch instead of one per key is what matters.
- **otter's single-key serial benchmark is not representative**: reading one key over and over from one goroutine keeps waking otter's maintenance goroutine, so `Get/hit/otter/serial` is about 170 to 190ns. Spread over many keys and goroutines (`Get/hit/otter/parallel`, `ZipfMixed/otter`) it is 4 to 5 times faster than v1's ristretto layer.
- **Writes**: a write whose stripe is free takes it without allocating. Writes spread over stripes are 42 to 50% faster than v1; writes to keys in one stripe, which queue for the stripe's lock, are on par with v1.
- `Get/hit/l2` is 37 to 47% slower than v1. Its time is mostly goroutine hand-offs (the fetch runs in its own goroutine so that callers can leave); the CPU spent inside cachex is small.
