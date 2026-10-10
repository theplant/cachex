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
| `Get/hit/map/serial` | fresh hit, in-memory layer | 17.8ns | 86.3ns | 17.7ns | 58.7ns | 0 → 0 |
| `Get/hit/map/parallel` | the same from 12 goroutines | 2.5ns | 11.3ns | 2.4ns | 11.4ns | 0 → 0 |
| `Get/hit/otter/parallel` | fresh hit, memory layer (v1: ristretto, v2: otter) | 126ns | 28.2ns | 105ns | 23.6ns | 0 → 0 |
| `Get/notfound-hit` | hit on a fresh not-found entry | 552ns | 86.0ns | 500ns | 57.9ns | 12 → 0 |
| `Get/stale-hit` | stale hit, starting a background refresh | 105ns | 74.0ns | 88.4ns | 70.7ns | 2 → 1 |
| `Get/miss` | full fetch: claim, source, backfill | 2.82µs | 2.13µs | 2.78µs | 2.06µs | 26 → 21 |
| `Get/miss/x-sync-baseline` | `sync.Map` + `x/sync/singleflight`, for scale | 274ns | 160ns | 263ns | 142ns | 7 → 4 |
| `Get/hit/l2` | first layer misses, second layer hits, backfill | 2.61µs | 3.90µs | 2.61µs | 3.92µs | 23 → 24 |
| `GetMany/hit/n=100/GetMany` | 100 keys, all hits | 11.6µs | 12.2µs | 10.1µs | 11.0µs | 14 → 14 |
| `GetMany/hit/n=100/loop-Get` | the same keys, `Get` in a loop | 2.40µs | 8.80µs | 2.40µs | 6.40µs | 0 → 0 |
| `GetMany/half-miss/n=100/GetMany` | 100 keys, half not in the source, nothing cached for them | 66.8µs | 55.2µs | 56.5µs | 43.2µs | 598 → 210 |
| `GetMany/half-miss/n=100/loop-Get` | the same keys, `Get` in a loop | 155µs | 108µs | 159µs | 105µs | 1500 → 900 |
| `HotKeyStampede` | 64 concurrent misses of one key; source calls per round = 1 in both | 85.7µs | 66.3µs | 65.9µs | 50.3µs | 546 → 151 |
| `SetDel/spread` | parallel `Set`/`Del` over 1024 keys, with a reader | 339ns | 267ns | 371ns | 244ns | 4 → 4 |
| `SetDel/same-stripe` | the same, all keys in one stripe | 370ns | 528ns | 283ns | 351ns | 6 → 7 |
| `ZipfMixed/map` | parallel Zipf reads over 10,000 keys, 10% not in the source | 262ns | 224ns | 182ns | 149ns | 4 → 2 |
| `ZipfMixed/otter` | the same over otter (v2 only) | | 230ns | | 194ns | 2 |

## What the numbers say

- **A hit costs more in v2, and most of it is reading the clock.** Every hit checks the entry's freshness, which needs the time: `time.Now()` alone costs about 30ns on macOS and 38ns in the Linux VM. v1's hit benchmark stored plain values with no freshness check; v1 users of `Entry[T]` paid for the clock too. The rest (finding the key's stripe, copying the entry) is about 25ns.
- **Not-found hits are nearly free now**: a not-found entry is a state of the entry in the layer, read once, and `ErrNotFound` is returned as is (no wrapping, no allocation). v1 read a second backend and formatted an error.
- **Misses and fetches are cheaper** (−25%, 21 allocations instead of 26); a stampede of 64 readers still calls the source once.
- **`GetMany` pays off only with misses or a remote layer.** On an in-memory layer with every key a hit it costs about 110 to 120ns per key, against 64 to 88ns for a loop of `Get`; with misses it is 17 to 30% faster than v1 and fetches once. Against Redis or a database, one round trip for the batch instead of one per key is what matters.
- **otter's single-key serial benchmark is not representative**: reading one key over and over from one goroutine keeps waking otter's maintenance goroutine, so `Get/hit/otter/serial` is about 165 to 200ns. Spread over many keys and goroutines (`Get/hit/otter/parallel`, `ZipfMixed/otter`) it is 4 to 5 times faster than v1's ristretto layer.
- **Writes to keys in one stripe are slower than v1** (+24 to +43%): such writes queue for the stripe's lock, and each write draws a random number for the jitter. Writes spread over stripes are 20 to 35% faster.
- `Get/hit/l2` is 50% slower than v1. Its time is mostly a goroutine hand-off (the fetch runs in its own goroutine so that callers can leave); the CPU spent inside cachex is small.
