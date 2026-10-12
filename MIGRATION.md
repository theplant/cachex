# Migrating from v1 to v2

English | [中文](MIGRATION_ZH.md)

v2 is a new major version: a new import path, a new API, and a new storage format. Cached data is disposable, so upgrading costs one cold start, not a data migration.

## Import path

```go
import "github.com/theplant/cachex/v2"
```

Backends moved to their own packages: `github.com/theplant/cachex/v2/ottercachex`, `bigcachex`, `rediscachex`, `gormcachex`, and `cachextest` for tests.

## API

| v1 | v2 |
|---|---|
| `Client[T]`, `NewClient(backend, upstream, opts...)` | `Cache[T]`, `New(source, layers, opts...)` |
| one `Client` per layer, chained through `upstream` | one `Cache` with `[]Layer[T]`, top layer first; each layer is `NewLayer(backend, layerOpts...)` |
| `Cache[T]` (backend interface) | `Backend[T]`, batch methods included; `SingleBackend[T]` + `Batched` for single-key stores |
| `Upstream[T]`, `UpstreamFunc[T]` | `Source[T]`, `SourceFunc[T]` |
| `ErrKeyNotFound`, `IsErrKeyNotFound(err)` | `ErrNotFound`, `errors.Is(err, cachex.ErrNotFound)`; the `Cached`/`CacheState` fields are gone |
| `Entry[T]`, `EntryWithTTL`, `WithStale`, `WithServeStale` | layer option `TTL(fresh, stale)`: a stale TTL above zero is serve-stale. Values are plain `T` |
| `NotFoundWithTTL`, `WithNotFound` (a second backend) | layer option `NotFoundTTL(fresh, stale)`, kept in the same backend |
| a custom staleness check on the value | `WithMaxAge(func(T) time.Duration)` for values that expire on their own; put a version into the key for "invalidate everything" |
| `WithFetchConcurrency` | none: the concurrent misses of a key always share one fetch |
| `WithFetchTimeout`, `WithLogger`, `WithDoubleCheck` | same names, no type parameter |
| `NowFunc`, `MockClock` | `WithNow(func() time.Time)`, `cachextest.Clock` |
| `DefaultFetchTimeout` and other `Default*` variables | constants |
| `Transform`, `JSONTransform`, `StringJSONTransform` | removed: byte-storing backends take a `Codec[T]` (default `DefaultCodec`); your own store of bytes implements `ByteBackend` and is wrapped with `Encoded(b, codec, logger)` |
| `RistrettoCache` | `ottercachex.Backend` (`MaximumSize` or `MaximumWeight` required) |
| `BigCache` | `bigcachex.New`, which returns a `cachex.Backend[T]` |
| `RedisCache`, `RedisCacheConfig.TTL` | `rediscachex.New`, which returns a `cachex.Backend[T]`; no TTL setting, see below |
| `GORMCache` | `gormcachex.New`, which returns a `*gormcachex.Backend[T]` with `Migrate` |
| `WithGORMTx`, `GetGORMTx` | removed: `gormcachex` never joins your transaction, see below |
| `SyncMap` | `cachextest.Map`, for tests only (unbounded) |
| — | `Cache.GetMany` with `BatchSource[T]`, `WithGetManyConcurrency` and `WithGetManyChunkSize`; `Cache.SetMany`, `Cache.DelMany`, `Cache.Close`, `IsShared` |
| — | `cachextest.TestBackend(t, newBackend, values)` and `cachextest.TestByteBackend(t, newBackend)`, contract tests for your own backends |

## Behavior

**Layers live in one `Cache`.** A key has one fetch across all layers: a miss in the top layer claims it, reads the lower layers in turn, then the source, and backfills every layer above where it was found. An entry copied up gets the upper layer's TTLs from the copy on, but is never fresher or longer lived than the entry below it, so data is never older than the lowest layer allows. *What to do:* build one `Cache` with all layers instead of chaining clients.

**Backends no longer have their own TTL.** Each entry carries when it turns stale and rotten, computed from the layer's TTLs; Redis and otter expire each entry natively at that time; the database table keeps rows until you delete them, and BigCache until its `LifeWindow` ends. *What to do:* drop backend TTL settings; set `TTL` on each layer.

**Jitter is on by default.** Each entry's fresh period is shortened at random by up to 10% (`DefaultJitter`), never lengthened. *What to do:* nothing, or `Jitter(0)` to turn it off.

**Writes go bottom up and never write the source.** `Set`, `Del`, `SetMany` and `DelMany` write the lowest layer first; in v1, each client wrote its own backend, then the next one down. If a layer fails, the key is dropped from that layer and every layer above it, and the error is returned. *What to do:* change the source first, then write through the same `Cache`.

**`Del` only invalidates.** v1 recorded a "does not exist" on `Del` when `WithNotFound` was configured, so a `Del` after an update hid the updated row until that record expired. v2 drops the key from every layer; the next read asks the source. *What to do:* nothing; if you relied on `Del` meaning "deleted", the next read records it.

**A key with a `}` but no hash tag is no longer supported.** For example `a}b`: on Redis Cluster it cannot share a slot with its lease. Whatever the layers, `Get`, `GetMany`, `Set` and `SetMany` fail it with `ErrUnsupportedKey`, and `Del` and `DelMany` do nothing. Keys with a hash tag (`{user:1}:profile`) and keys with only a `{` are not affected. *What to do:* replace the `}` in the key, or give it a hash tag.

**Fetches outlive their caller.** A fetch is shared by every request of the key, so it does not end with the caller that started it; its ctx keeps the caller's values but not its cancellation, and `IsShared(ctx)` reports it. *What to do:* nothing; a backend you write must not use caller state, such as a transaction, from such a ctx.

**A fetch that panics or calls `runtime.Goexit` answers every waiter with an error.** In v1, waiters of a Goexit'ed fetch waited until their own ctx ended.

**`DoubleCheckAuto` re-reads the first layer only if this `Cache` wrote the key's stripe since the request read it.** In v1 it was on whenever `WithNotFound` was configured. Use `DoubleCheckEnabled` to see what other processes wrote to a shared first layer.

**Data that does not decode is a miss.** In v1, a value the codec could not read (after a type change, say) failed every `Get` until it expired. v2's byte-storing backends (through `cachex.Encoded`) drop it, log a warning, and the read fetches anew.

**The storage format changed.** v1 data in Redis or a table is not readable by v2 and reads as misses. *What to do:* use a new `KeyPrefix` (or Redis database), and a new table for `gormcachex` (its columns changed: `value` holds the encoded entry, `expires_at` was added); run `Migrate`.

**`gormcachex` never joins your transaction.** v1's `WithGORMTx` put cache writes in your transaction; v2 has no such option, and every statement commits on its own. A write held in a transaction keeps its row locks across `Cache` calls while the `Cache` orders writes of a key in the process, and the two can wait for each other forever. *What to do:* change the source in your transaction, then call `Set`/`Del` after it commits.

**Backends expire entries by the wall clock.** `WithNow` (v1's `NowFunc`) sets the clock the `Cache` judges entries by, but Redis and otter drop an entry once its `ExpiresAt` has passed by `time.Now`, and the byte-storing backends do not write an entry already past it by `time.Now`. *What to do:* in tests that use a `cachextest.Clock` with such a backend, start the clock at `time.Now()`.

**MySQL needs 8.0.17 or later; MariaDB is not supported.** `Migrate` creates the table with a `utf8mb4_0900_bin` key column and rejects an existing table whose key column uses another collation, with the `ALTER TABLE` statement that converts it. Only each database's default isolation level is supported.

**Batch writes are best effort.** `SetMany`/`DelMany` of the `Cache` and of every backend try every key and list the failed ones in a `*BatchError`.

**`GetMany` claims its keys a chunk at a time.** By default all of a `GetMany`'s misses are one chunk, claimed up front: with a source that is not a `BatchSource`, a concurrent `Get` of a key queued in a large `GetMany` waits for its turn. *What to do:* implement `BatchSource` on sources used with `GetMany`, or set `WithGetManyChunkSize` to about `WithGetManyConcurrency`: a key whose chunk has not started is not claimed, so a `Get` of it fetches it at once.

**`Close` waits for the fetches in progress and their backfills, background refreshes, and the invalidations of writes that gave up waiting.** *What to do:* call it before closing the backends.

**A read returns before its backfill.** A fetch answers its callers first and writes the layers above afterwards; a read that comes meanwhile gets the same answer without fetching again. So right after `Get` returns, a layer may not hold the value yet. *What to do:* nothing, unless a test looks at a backend right after a read, or the process exits right after a read (a command-line tool): call `Close` first.

**The benchmark numbers changed.** v1's `BENCHMARK.md` measured a simulation dominated by sleeps; see the new [BENCHMARK.md](BENCHMARK.md).
