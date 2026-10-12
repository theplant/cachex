# cachex

> A multi-layer read-through cache for Go: concurrent misses of a key are fetched once, writes reach every layer in one order, and every layer keeps its own freshness.

[![Go Reference](https://pkg.go.dev/badge/github.com/theplant/cachex/v2.svg)](https://pkg.go.dev/github.com/theplant/cachex/v2)
[![License](https://img.shields.io/github/license/theplant/cachex)](LICENSE)

English | [中文](README_ZH.md)

A `Cache` reads its layers top down (memory, then Redis, then a database table, say) and asks the source only when no layer has a usable entry. Upgrading from v1? See [MIGRATION.md](MIGRATION.md).

## Install

```bash
go get github.com/theplant/cachex/v2
```

## Quick start

One in-memory layer over a source:

```go
mem, err := ottercachex.New[*Product](ottercachex.Config[*Product]{MaximumSize: 100_000})
if err != nil {
    log.Fatal(err)
}

source := cachex.SourceFunc[*Product](func(ctx context.Context, id string) (*Product, error) {
    p, err := loadProduct(ctx, id) // your database query
    if errors.Is(err, gorm.ErrRecordNotFound) {
        return nil, cachex.ErrNotFound // "does not exist" is ErrNotFound
    }
    return p, err
})

products := cachex.New(source, []cachex.Layer[*Product]{
    cachex.NewLayer[*Product](mem, cachex.TTL(time.Minute, 10*time.Minute)),
})
defer products.Close()

p, err := products.Get(ctx, "42")
switch {
case errors.Is(err, cachex.ErrNotFound):
    // no such product
case err != nil:
    // a layer or the source failed
}
```

`TTL(fresh, stale)`: for a minute the entry is returned as is; for ten more minutes it is still returned at once while a background refresh fetches it again; after that it is not served.

## Two layers

Memory in front of Redis, both in front of the database. Each layer has its own TTLs; an entry copied from Redis into memory is kept for the memory layer's TTL, but never fresher or longer than the entry in Redis:

```go
mem, _ := ottercachex.New[*Product](ottercachex.Config[*Product]{MaximumSize: 100_000})
shared := rediscachex.New[*Product](rediscachex.Config[*Product]{Client: rdb, KeyPrefix: "product:v1:"})

products := cachex.New(source, []cachex.Layer[*Product]{
    cachex.NewLayer[*Product](mem,
        cachex.TTL(30*time.Second, time.Minute),
        cachex.NotFoundTTL(5*time.Second, 0)), // remember "does not exist" for 5s
    cachex.NewLayer[*Product](shared,
        cachex.TTL(5*time.Minute, time.Hour),
        cachex.NotFoundTTL(30*time.Second, 0)),
}, cachex.WithFetchTimeout(3*time.Second))
```

Values that expire on their own, such as tokens, get a per-value cap with `WithMaxAge`; it works on types you cannot add methods to:

```go
tokenMem, _ := ottercachex.New[*oauth2.Token](ottercachex.Config[*oauth2.Token]{MaximumSize: 10_000})
tokens := cachex.New(tokenSource, []cachex.Layer[*oauth2.Token]{ // tokenSource is a cachex.Source[*oauth2.Token]
    cachex.NewLayer[*oauth2.Token](tokenMem, cachex.TTL(time.Hour, 0)),
}, cachex.WithMaxAge(func(t *oauth2.Token) time.Duration {
    return time.Until(t.Expiry) - time.Minute // never serve a token about to expire
}))
```

## Writes

Change the source first, then write through the same `Cache`:

```go
if err := db.WithContext(ctx).Save(p).Error; err != nil { // 1. the source
    return err
}
return products.Set(ctx, p.ID, p) // 2. every layer, bottom up
```

`Set` writes every layer, bottom up; `Del` drops the key from every layer, so the next read asks the source. Neither writes the source. A read that started before the write cannot put the old value back after it. `SetMany` and `DelMany` do the same for many keys.

## Batch reads

`GetMany` reads many keys with one call per layer. If the source also implements `BatchSource`, the misses are fetched in one call too:

```go
// GetMany makes productSource a cachex.BatchSource: a key it does not return
// does not exist.
func (s productSource) GetMany(ctx context.Context, ids []string) (map[string]*Product, error) {
    var rows []*Product
    if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err != nil {
        return nil, err // every key failed
    }
    out := make(map[string]*Product, len(rows))
    for _, p := range rows {
        out[p.ID] = p
    }
    return out, nil
}

found, err := products.GetMany(ctx, []string{"1", "2", "3"})
var be *cachex.BatchError
if errors.As(err, &be) {
    // be.Errors lists the keys that failed; found holds every other key
}
```

## Several instances

When several processes share a Redis layer, one of them may read a value from the source, another may then change the source and call `Set`, and the first may write its old value into Redis after that write. Every instance then copies the old value into its memory layer until the Redis entry rots. Turn leases on to prevent it:

```go
shared := rediscachex.New[*Product](rediscachex.Config[*Product]{Client: rdb, KeyPrefix: "product:v1:", Leases: true})
```

`gormcachex` has the same `Leases` option, for a table several instances share (after `Migrate`). A fetch leases each key it misses in Redis, a write of the key ends the lease, and the fetch writes Redis only if it still holds its lease. A miss whose lease another instance holds is still answered, without writing Redis; a background refresh then leaves the key to that instance. Leases do not reach other instances' memory layers. For that, turn on the invalidation broadcast: after a write, each instance drops the written keys from its memory layers.

```go
products := cachex.New(source, layers, cachex.WithBroadcast(
    rediscachex.NewBroadcast(rediscachex.BroadcastConfig{Client: rdb, KeyPrefix: "product:v1:"})))
```

It uses Redis Pub/Sub (Redis Cluster too); without Redis but with PostgreSQL, `gobuscachex.NewBroadcast` does the same over go-bus, with messages polled from a queue per instance. Messages published while an instance is disconnected are lost, so each time it subscribes again it clears its memory layers: the cost of a lost message is a cold memory layer, not an old value. Only layers whose backend lives in the process are touched (`ottercachex`, `bigcachex`, or your own backend implementing `cachex.LocalBackend`); a Cache with a broadcast needs one. A failed publish is logged, not returned. With leases and the broadcast on, the Redis layer's TTLs can be long and a write shows on every instance as soon as the message arrives.

## Options

| Option | Default | What |
|---|---|---|
| `TTL(fresh, stale)` (layer) | required | How long an entry is fresh, then how long it is still served while refreshed |
| `NotFoundTTL(fresh, stale)` (layer) | `0, 0`: not recorded | The same for "does not exist" |
| `Jitter(ratio)` (layer) | `0.1` | Shortens each entry's fresh period by up to this share, so entries written together do not turn stale or rotten together |
| `WithMaxAge(func(T) time.Duration)` | none | Caps how long a value is kept, in every layer; called for fetched values and for those `Set`/`SetMany` write |
| `WithFetchTimeout(d)` | 60s | Bounds, each on its own: a fetch's double check, each lower layer read with its backfill, each source call with its backfill, and the invalidation after a failed write |
| `WithGetManyConcurrency(n)` | 16 | How many source requests one `GetMany` has in flight |
| `WithGetManyChunkSize(n)` | 0: one chunk | Fetches a `GetMany`'s misses in chunks of at most n keys, each claimed when it starts; a `Get` of a key whose chunk has not started does not wait |
| `WithDoubleCheck(mode)` | `DoubleCheckAuto` | When a request that claimed a fetch re-reads the first layer first |
| `WithLogger(l)` | `slog.Default()` | Logs failures that do not fail a call (a backfill, an invalidation, a background refresh) and panics in fetches |
| `WithNow(f)` | `time.Now` | The clock entries are computed and judged by (backends still expire entries by the wall clock); `cachextest.Clock` is a manual one for tests |

## Backends

| Package | Stores | Notes |
|---|---|---|
| `ottercachex` | values in memory | `MaximumSize` (entries) or `MaximumWeight` with `Weigher` is required; each entry expires at its own time |
| `bigcachex` | encoded entries in memory | For millions of entries, when the garbage collector shows in profiles; every read decodes |
| `rediscachex` | encoded entries in Redis or Redis Cluster | Native expiry per entry; batch calls are pipelines of `ChunkSize` keys; `Leases` for a Redis several instances share; `NewBroadcast` for the invalidation broadcast |
| `gormcachex` | encoded entries in a table | Call `Migrate`; MySQL 8.0.17+ only; see Pitfalls for keys, isolation and expired rows; `Leases` for a table several instances share |
| `gobuscachex` | nothing: the invalidation broadcast over go-bus | PostgreSQL only; for deployments with PostgreSQL and no Redis; each instance subscribes a queue of its own, kept alive by heartbeats |
| `cachextest` | values in memory, unbounded | For tests only, with `Clock`, `NewLeasingMap` (a shared map with leases, for Caches standing for instances), `NewBus` (an in-memory broadcast that can lose messages), and `TestBackend`, `TestByteBackend`, `TestLeaser` and `TestBroadcast`, contract tests for your own backends and transports |

`rediscachex.New` and `bigcachex.New` return a `cachex.Backend[T]`; `gormcachex.New` returns a `*gormcachex.Backend[T]`, which is one and adds `Migrate`.

Your own store implements `Backend[T]`, or `SingleBackend[T]` wrapped with `cachex.Batched`. A store of bytes implements `cachex.ByteBackend` and is wrapped with `cachex.Encoded`, which encodes entries with a `Codec`, treats data that does not decode as a miss, and deletes an entry already rotten instead of writing it.

## Pitfalls

Each item: the mistake, what happens, what to do.

**Source**

- **Returning `nil, nil` for a missing key.** The zero value is cached as a real value. Return `cachex.ErrNotFound` (wrapping it is fine).
- **Returning `ErrNotFound` from `BatchSource.GetMany`.** Every key of the call reads as missing but nothing is cached, so each read asks again. Leave missing keys out of the map; for some keys failing, return a `*cachex.BatchError` as is, not wrapped.
- **Relying on the caller's cancellation inside the source.** A fetch is shared by every caller waiting for the key, so its ctx does not end when one caller leaves; it is bounded by `WithFetchTimeout` instead, and keeps the caller's values. Do not use caller state from it, such as a database transaction (`cachex.IsShared(ctx)` tells you it is a fetch).
- **Calling the same `Cache` for the same key from inside the source.** The call waits for the fetch it is part of and fails at the fetch timeout.
- **A source without `GetMany` behind large `GetMany` calls.** Keys are fetched one by one (`WithGetManyConcurrency` at a time), and by default a concurrent `Get` of a queued key waits for its turn. Implement `BatchSource`, or set `WithGetManyChunkSize` to about `WithGetManyConcurrency`.

**Reads**

- **Treating `err != nil` from `GetMany` as "nothing found".** The map holds every key that succeeded; only the keys in the `*BatchError` failed. A key absent from both does not exist.
- **Testing presence with `m[k] == nil`.** A cached nil is a value. Use `v, ok := m[k]`.
- **Expecting a stale value never to be returned.** With a stale TTL above zero, a value past its fresh TTL is returned at once and refreshed in the background. Use a zero stale TTL where that is not acceptable.
- **Values that expire on their own (tokens, signed URLs) cached for the layer TTL.** Use `WithMaxAge` so an entry never outlives the value.
- **Expecting a broken layer to be skipped.** If a layer cannot be read, the read fails rather than sending every request to the source.
- **Checking a backend right after `Get` returns.** The answer is returned before it is written into the layers. Call `Close` first in tests.
- **A key with a `}` but no hash tag** (such as `a}b`). Such keys are not supported: reads and writes fail with `ErrUnsupportedKey`. Give it a hash tag (`{user:1}:profile`) or drop the `}`.

**Writes**

- **Writing the cache before the source, or only the cache.** `Set` and `Del` never write the source. Change the source first, then call `Set` or `Del` on the same `Cache`; the other order lets a read in between cache the old value again.
- **Expecting a write in one process to reach another's memory layer.** It does not; there only the TTLs bound how long old data lives. Turn on the invalidation broadcast, or set the memory layer's TTLs to what you can accept; turn leases on for a shared Redis (see Several instances), or its TTLs bound old data too.
- **Expecting `Del` to record that the key no longer exists.** It only drops the entries; the next read asks the source.
- **Ignoring an error from `Set` or `Del`.** The layers below the failed one may hold the new value; the key is dropped from the failed layer and those above (best effort). The source is as you left it.

**Values and backends**

- **Modifying a value you got from an in-memory backend** (`ottercachex`, `cachextest.Map`). It is the stored value itself, shared with every reader. Treat values as read-only, or copy them.
- **Changing a value type incompatibly and keeping `KeyPrefix`.** JSON decodes leniently: a renamed field reads back as a zero value. Change `KeyPrefix` with the type.
- **`ottercachex` without a bound.** `New` fails: set `MaximumSize`, or `MaximumWeight` with a `Weigher`.
- **`bigcachex` with a `LifeWindow` shorter than the layer's TTLs.** Entries disappear early. Use it only for millions of entries where GC shows in profiles; every read decodes.
- **`gormcachex` without `Migrate`, or a MySQL table not in `utf8mb4_0900_bin`.** `Migrate` creates the table, and rejects an existing MySQL table whose key column uses any other collation. Keys, `KeyPrefix` included, are at most 255 characters of valid UTF-8 without NUL; any other key always misses and fails to be written. Expired rows are not deleted for you. Only each database's default isolation level is supported.
- **Calling `Set`/`Del` inside the transaction that changes the source.** `gormcachex` never joins it: every statement commits on its own, so a rollback does not undo the cache. Call them after the commit.
- **A `cachextest.Clock` far behind the wall clock, with a backend that expires entries.** Backends expire entries by the wall clock whatever `WithNow` says, so they drop entries computed by such a clock at once and every read misses. Start the clock at `time.Now()`, or use `cachextest.Map`.

- **`gobuscachex` without PostgreSQL.** go-bus runs on PostgreSQL only: with MySQL, it would take a PostgreSQL of its own just for the broadcast. Use it when the database is PostgreSQL and there is no Redis; otherwise use `rediscachex.NewBroadcast`.

**Lifecycle**

- **Closing the backends while the `Cache` still works.** Call `Close` first: it waits for the fetches in progress and their backfills, background refreshes, and the invalidations of writes that gave up waiting.
- **Exiting right after a read** (a command-line tool, a cron job). A read returns before its answer is written into the layers, so the process can end before Redis or the table is filled, and the next run fetches again. Call `Close` before exiting.

## Documentation

- [FAQ](docs/faq.md) ([中文](docs/faq_ZH.md))
- [Migrating from v1](MIGRATION.md)
- [Benchmarks](BENCHMARK.md)
- Design notes, in Chinese: [design overview](docs/design.md), [glossary](GLOSSARY.md), [decisions (ADRs)](docs/adr/)

## License

[MIT](LICENSE)
