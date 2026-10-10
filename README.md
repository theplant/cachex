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
tokens := cachex.New(source, []cachex.Layer[*oauth2.Token]{
    cachex.NewLayer(mem, cachex.TTL(time.Hour, 0)),
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

## Options

| Option | Default | What |
|---|---|---|
| `TTL(fresh, stale)` (layer) | required | How long an entry is fresh, then how long it is still served while refreshed |
| `NotFoundTTL(fresh, stale)` (layer) | `0, 0`: not recorded | The same for "does not exist" |
| `Jitter(ratio)` (layer) | `0.1` | Shortens each entry's fresh period by up to this share, so entries written together do not expire together |
| `WithMaxAge(func(T) time.Duration)` | none | Caps how long a value is kept, in every layer |
| `WithFetchTimeout(d)` | 60s | Bounds each call a fetch makes: a lower layer read, a source call, a backfill |
| `WithFetchesPerKey(n)` | 1 | How many fetches of one key may run at once; 1 merges every concurrent miss |
| `WithGetManyConcurrency(n)` | 16 | How many source requests one `GetMany` has in flight |
| `WithGetManyChunkSize(n)` | 0: one call | Splits a `BatchSource` call into chunks of at most n keys |
| `WithDoubleCheck(mode)` | `DoubleCheckAuto` | When a request that claimed a fetch re-reads the first layer first |
| `WithLogger(l)` | `slog.Default()` | Logs failures that do not fail a call (a backfill, a background refresh) |
| `WithNow(f)` | `time.Now` | The clock; `cachextest.Clock` is a manual one for tests |

## Backends

| Package | Stores | Notes |
|---|---|---|
| `ottercachex` | values in memory | `MaximumSize` (entries) or `MaximumWeight` with `Weigher` is required; each entry expires at its own time |
| `bigcachex` | encoded entries in memory | For millions of entries, when the garbage collector shows in profiles; every read decodes |
| `rediscachex` | encoded entries in Redis or Redis Cluster | Native expiry per entry; batch calls are pipelines of `ChunkSize` keys |
| `gormcachex` | encoded entries in a table | Call `Migrate`; MySQL needs 8.0.17+ and a `utf8mb4_0900_bin` key column; tested at each database's default isolation level only; keys up to 255 characters including `KeyPrefix`; expired rows are not deleted for you |
| `cachextest` | values in memory, unbounded | For tests only, with `Clock` and `TestBackend`, a contract test for your own backends |

Your own store implements `Backend[T]`, or `SingleBackend[T]` wrapped with `cachex.Batched`. A store of bytes encodes entries with `cachex.EncodeEntry`/`DecodeEntry` and a `Codec`.

## Pitfalls

Each item: the mistake, what happens, what to do.

**Source**

- **Returning `nil, nil` for a missing key.** The zero value is cached as a real value. Return `cachex.ErrNotFound` (wrapping it is fine).
- **Returning `ErrNotFound` from `BatchSource.GetMany`.** Every key of the call reads as missing but nothing is cached, so each read asks again. Leave missing keys out of the map; for some keys failing, return a `*cachex.BatchError` as is, not wrapped.
- **Relying on the caller's cancellation inside the source.** A fetch is shared by every caller waiting for the key, so its ctx does not end when one caller leaves; it is bounded by `WithFetchTimeout` instead, and keeps the caller's values. Do not use caller state from it, such as a database transaction (`cachex.IsShared(ctx)` tells you it is a fetch).
- **Calling the same `Cache` for the same key from inside the source.** The call waits for the fetch it is part of and fails at the fetch timeout.
- **A source without `GetMany` behind large `GetMany` calls.** Keys are fetched one by one (`WithGetManyConcurrency` at a time), and a concurrent `Get` of a queued key waits for its turn. Implement `BatchSource`.

**Reads**

- **Treating `err != nil` from `GetMany` as "nothing found".** The map holds every key that succeeded; only the keys in the `*BatchError` failed. A key absent from both does not exist.
- **Testing presence with `m[k] == nil`.** A cached nil is a value. Use `v, ok := m[k]`.
- **Expecting a stale value never to be returned.** With a stale TTL above zero, a value past its fresh TTL is returned at once and refreshed in the background. Use a zero stale TTL where that is not acceptable.
- **Values that expire on their own (tokens, signed URLs) cached for the layer TTL.** Use `WithMaxAge` so an entry never outlives the value.
- **Expecting a broken layer to be skipped.** If a layer cannot be read, the read fails rather than sending every request to the source.
- **Checking a backend right after `Get` returns.** The answer is returned before it is written into the layers. Call `Close` first in tests.

**Writes**

- **Writing the cache before the source, or only the cache.** `Set` and `Del` never write the source. Change the source first, then call `Set` or `Del` on the same `Cache`; the other order lets a read in between cache the old value again.
- **Expecting a write in one process to reach another's memory layer.** It does not; across instances only the TTLs bound how long old data lives. Set each layer's TTLs to what you can accept.
- **Expecting `Del` to record that the key no longer exists.** It only drops the entries; the next read asks the source.
- **Ignoring an error from `Set` or `Del`.** The layers below the failed one may hold the new value; the key is dropped from the failed layer and those above (best effort). The source is as you left it.

**Values and backends**

- **Modifying a value you got from an in-memory backend** (`ottercachex`, `cachextest.Map`). It is the stored value itself, shared with every reader. Treat values as read-only, or copy them.
- **Changing a value type incompatibly and keeping `KeyPrefix`.** JSON decodes leniently: a renamed field reads back as a zero value. Change `KeyPrefix` with the type.
- **`ottercachex` without a bound.** `New` fails: set `MaximumSize`, or `MaximumWeight` with a `Weigher`.
- **`bigcachex` with a `LifeWindow` shorter than the layer's TTLs.** Entries disappear early. Use it only for millions of entries where GC shows in profiles; every read decodes.
- **`gormcachex` without `Migrate`, or a MySQL table not in `utf8mb4_0900_bin`.** `Migrate` creates the table, and rejects an existing MySQL table whose key column compares case-insensitively. Keys, `KeyPrefix` included, are at most 255 characters. Expired rows are not deleted for you. Only each database's default isolation level is supported.
- **`gormcachex.WithTx` expecting every write to join the transaction.** `Set`/`Del` with that ctx do; the backfill of a fetch never does.
- **`cachex.NewLayer(backend, …)` failing to compile with a concrete backend.** Go cannot infer `T` from it: write `cachex.NewLayer[*Product](backend, …)`.

**Lifecycle**

- **Closing the backends while the `Cache` still works.** Call `Close` first: it waits for the fetches, their backfills, background refreshes and the invalidations of writes that gave up.

## Documentation

- [FAQ](docs/faq.md) ([中文](docs/faq_ZH.md))
- [Migrating from v1](MIGRATION.md)
- [Benchmarks](BENCHMARK.md)
- Design notes, in Chinese: [design overview](docs/design.md), [glossary](GLOSSARY.md), [decisions (ADRs)](docs/adr/)

## License

[MIT](LICENSE)
