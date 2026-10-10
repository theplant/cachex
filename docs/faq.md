# FAQ

English | [中文](faq_ZH.md)

Each answer gives the conclusion first, then the reasoning. The design notes, [design.md](design.md) and its pages (in Chinese), are the reference for details.

## Usage

### Some values expire on their own, such as tokens. How do I keep them from being served too long?

Use `WithMaxAge(func(T) time.Duration)`. It is called once when a value is fetched, and caps how long the value is kept in every layer; zero or less means the value is not cached. It takes a function rather than a method on `T`, so it works on types from other packages. For "everything older than version N is invalid", put the version into the key instead.

### What is the difference between the fresh and the stale TTL?

Both are set per layer with `TTL(fresh, stale)` and count from when the entry is written into that layer; an entry copied from a lower layer is never fresher or longer lived than the one below. During the fresh TTL an entry is returned as is. The stale TTL is an **additional** period: a read in it gets the entry at once while a background refresh fetches it again. After `fresh + stale` the entry is rotten and never served. `Jitter` shortens each entry's fresh period at random (by up to 10% by default) so entries written together do not expire together. Backends need no TTL of their own: Redis and otter expire each entry when it turns rotten, and rotten entries are never served from any backend.

### Should I cache every database query?

No. Cache data that is read often and changes rarely. Poor candidates are:

- data that changes very often (needs to be fresh within a second);
- per-user data with very high cardinality;
- large objects that are expensive to hold in memory.

### What do I do after changing the data source? Do I take the striped lock first?

No; the striped lock is internal and you cannot take it. Just keep one order: **change the source first, then call `Set` or `Del` through the same `Cache`**. In the opposite order (`Del` first, then change the source), a read that fetches in between gets the old value and legitimately backfills it. See [design/write-order.md](design/write-order.md) (Chinese).

### A cached value is nil. How do I tell it from "does not exist"?

A cached nil (a nil pointer, for example) is a hit:

- `Get` returns `(nil, nil)`, not `ErrNotFound`;
- `GetMany`'s map holds the key, with a nil value.

So test presence with `v, ok := m[k]`, not with `m[k] == nil`.

### Why are keys that do not exist missing from `GetMany`'s `BatchError`?

In a batch read, **not existing is not an error**: a key that does not exist is simply absent from the map, and `BatchError` lists only the keys that failed. A cache's batch read almost always has misses; if they counted as errors, `err` would almost never be nil and every caller would have to filter them out. Each key ends in exactly one of three outcomes:

| Outcome | `Get` | `GetMany` |
|---|---|---|
| found | `(value, nil)` | in the map |
| does not exist | `errors.Is(err, ErrNotFound)` | neither in the map nor in the `BatchError` |
| failed | any other error | in the `BatchError` |

See [ADR 0004](adr/0004-batch-result-shape.md) (Chinese).

### Why does `Get` still return `ErrNotFound`? Isn't that inconsistent with `GetMany`?

Both report the same three outcomes, each in its own way. `Get` returns a single value, so it cannot say "does not exist" by leaving something out: the value itself may legitimately be nil, and when `T` is an int, the zero value cannot mean "absent" at all. It is the same in `database/sql`: `QueryRow` returns `ErrNoRows` when nothing matches, while `Query` just returns no rows.

### Do I need to call `Close`?

Call it at shutdown, before closing the backends. It waits for the fetches and background refreshes in progress, whose backfills still write the backends, and starts no more refreshes. It does not close the backends. A `Cache` that is never closed leaks nothing once its last fetch ends.

### What happens to cached data from v1 after upgrading?

v2 cannot read it: a backend that stores bytes treats data that does not decode as a miss, drops it and logs a warning, so the first reads fetch from the source. Use a new `KeyPrefix` (and, for `gormcachex`, a new table) to skip that. See [MIGRATION.md](../MIGRATION.md).

## Singleflight and the double-check

### How is cache stampede prevented?

In two layers:

1. **Merged fetches** (primary): concurrent misses of one key fetch it once, across all layers; the other requests wait for that result. `WithFetchesPerKey(n)` allows n concurrent fetches of one key (default 1), trading some load on the layers below for throughput. See [design/singleflight.md](design/singleflight.md) (Chinese).
2. **Double-check** (supplementary): request B reads a miss before request A's fetch backfills, but claims the key only after A's fetch has finished; B reads the first layer again instead of fetching again.

### Is the double-check worth it? Is it on by default?

By default it runs on demand (`DoubleCheckAuto`): it re-reads only when this `Cache` wrote the key's stripe after the request read the first layer. Measured:

- with a 1 ms first-layer read and a hot key, source calls drop from 30 to 19 (18 when always on);
- cold keys see almost no extra reads (1.005 reads per `Get`, against 2 when always on);
- with an in-memory first layer it makes no difference.

Use `DoubleCheckEnabled` when you need to see values other processes wrote to a shared first layer. See [ADR 0008](adr/0008-on-demand-double-check.md) and [research/2026-10-double-check.md](research/2026-10-double-check.md) (Chinese).

### What happens if the source panics or calls `runtime.Goexit`?

Every waiter of that fetch gets an error; none hangs:

- a panic gives `cachex: panic during fetch: …`, and an ERROR log with the stack;
- `Goexit` gives `cachex: fetch exited without returning (runtime.Goexit)`.

The latter differs from `x/sync/singleflight` (where waiters wait until their own ctx ends), on purpose. See [ADR 0006](adr/0006-goexit-publishes-an-error.md) (Chinese).

## Write order and the striped lock

### What is the point of guaranteeing the write order? The TTL applies anyway.

The TTL bounds how long old data lives **when you do not write**. `Set`/`Del` is you asking for "the new value, now". The order guarantee makes sure that request is not quietly undone by a fetch that started earlier. Without it, after `Del` returns successfully, the old value can still be backfilled and live for the whole TTL, and a user can see data turn new and then old again. See [design/write-order.md](design/write-order.md) (Chinese).

### Does a read take write locks?

No. A read's backfill holds the stripe for reading only, and is skipped (a spare miss) if a write holds it. Write locks are taken only when your code calls `Set`, `Del`, `SetMany` or `DelMany`. Reads never wait for writes.

### How far does one slow operation reach?

- A slow `Set`/`Del` holds only its own stripe (one of 4096);
- a large batch backfill holds the stripes of all its keys while it writes (about 22% for 1,000 keys), and writes to those stripes queue behind it;
- reads never wait.

For batches well over 1,000 keys from a `BatchSource`, `WithGetManyChunkSize` makes each chunk backfill on its own and hold only its own stripes; chunks run concurrently, so to limit the stripes held at once, also lower `WithGetManyConcurrency`. A smaller backend `ChunkSize` does not help. See [design/write-order.md](design/write-order.md) (Chinese).

### With several instances deployed, is the write order still guaranteed?

No. The striped lock works within one process only:

- other instances' memory layers do not see your write and keep serving the old value until it turns rotten;
- another instance can backfill an old value into the shared Redis.

TTLs are the backstop for now, so set every layer's TTLs to how long you can accept old data. Redis leases and invalidation broadcasts are in the [to-do list](todo.md) (Chinese). See [design/consistency.md](design/consistency.md) (Chinese).

## Batch reads

### Does `GetMany` return keys one by one, or all together?

All together. Fresh values, stale values and fetched values all go into one result, returned at once. Stale keys are refreshed separately in the background.

### How long does a concurrent `Get` wait for a key that `GetMany` is fetching?

`GetMany` claims every key it fetches at once, so a `Get` of one of them waits for that key's result:

- with a `BatchSource`, until the call (or, with `WithGetManyChunkSize`, the chunk) that carries the key returns;
- without one, until the key's turn comes among `WithGetManyConcurrency` source calls at a time.

Implement `BatchSource` on sources you use with large `GetMany` calls.

### What is the difference between `WithFetchesPerKey` and `WithGetManyConcurrency`?

- The former bounds how many fetches **one key** can have at once;
- the latter bounds how many requests **one `GetMany`** sends to the source at once (`Get` calls of a plain source, or chunk calls of a `BatchSource`).

## Backends

### Why must I set `MaximumSize` for `ottercachex`?

An in-memory layer without a bound grows with every key ever read, until the process runs out of memory. `ottercachex` therefore refuses a config without `MaximumSize` (a number of entries) or `MaximumWeight` with a `Weigher` (say, bytes). When it is full, otter evicts the entries least likely to be read again.

### When should I use `bigcachex` instead of `ottercachex`?

When the memory layer holds millions of entries and the garbage collector shows in CPU profiles. The garbage collector scans every live pointer, so millions of cached structs cost it work on every cycle; `bigcachex` stores encoded bytes it does not scan. The price is decoding on every read, which `ottercachex` does not do. Below a few hundred thousand entries, use `ottercachex`. See [BENCHMARK.md](../BENCHMARK.md).

### What does `gormcachex` need on MySQL?

- **MySQL 8.0.17 or later**, not MariaDB: `Migrate` creates the table with a `utf8mb4_0900_bin` key column, so keys compare case and trailing spaces exactly, and checks the version first.
- **An existing table** needs its `key` column in `utf8mb4_0900_bin`; otherwise `Migrate` returns an error that includes the statement to convert it (`ALTER TABLE <table> CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`). See [design/backends.md](design/backends.md) (Chinese).

### Do concurrent writes to `gormcachex` from several instances deadlock?

They rarely fail: writes lock rows in byte order, and the occasional deadlock (MySQL's gap locks) is retried automatically (at most 5 attempts). When the attempts run out, the error is returned; a failed backfill is only logged at WARN and the next read fetches again. Inside your own transaction (`gormcachex.WithTx`) nothing is retried, since the deadlock has rolled the whole transaction back. See [ADR 0010](adr/0010-gorm-deadlock-ordering-and-retry.md) (Chinese).

### Does `gormcachex` need a particular isolation level?

Only each database's default isolation level is supported and tested (REPEATABLE READ on MySQL, READ COMMITTED on PostgreSQL, SERIALIZABLE on SQLite). cachex sets no isolation level; each chunk is written by one autocommit statement. Do not change the default isolation level of the connection the cache table uses.

### My key column is already `utf8mb4_0900_bin`. Do I still need the deadlock retry?

Yes. `utf8mb4_0900_bin` makes rows lock in byte order, which removes most deadlocks; the rest come from gap locks under MySQL's default isolation level, which ordering cannot remove, so they are retried. On PostgreSQL no deadlock was measured once the order is uniform. A retry happens only after a statement has failed as a deadlock victim, so it costs nothing when there is no deadlock. See [design/backends.md](design/backends.md) (Chinese).
