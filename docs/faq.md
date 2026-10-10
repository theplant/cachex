# FAQ

English | [中文](faq_ZH.md)

Each answer gives the conclusion first, then the reasoning. The design notes, [design.md](design.md) and its pages (in Chinese), are the reference for details.

## Usage

### When should I use `Entry[T]`, and when a custom staleness check?

Use `Entry[T]` with `EntryWithTTL(freshTTL, staleTTL)` when staleness depends on time only. When it depends on your data (a `version` field in the value, say), pass your own check with `WithStale`. See [design/read-path.md](design/read-path.md#新鲜度) (Chinese).

### What is the difference between the fresh and the stale TTL?

During the fresh TTL a value is returned as is. The stale TTL is an **additional** period after it: with `WithServeStale`, a read in that period gets the old value at once while it is refreshed in the background. An entry lives `freshTTL + staleTTL` in total. Give the backend's own TTL at least that much, or entries disappear before their stale period begins.

### Should I cache every database query?

No. Cache data that is read often and changes rarely. Poor candidates are:

- data that changes very often (needs to be fresh within a second);
- per-user data with very high cardinality;
- large objects that are expensive to hold in memory.

### What do I do after changing the data source? Do I take the stripe lock first?

No; the stripe lock is internal and you cannot take it. Just keep one order: **change the data source first, then call `Set` or `Del` through the same Client**. In the opposite order (`Del` first, then change the source), a read that fetches in between gets the old value and legitimately backfills it. See [design/write-order.md](design/write-order.md#使用方式改数据源时要做什么) (Chinese).

### A cached value is nil. How do I tell it from "does not exist"?

A cached nil (a nil pointer, for example) is a hit:

- `Get` returns `(nil, nil)`, not `ErrKeyNotFound`;
- `GetMany`'s map holds the key, with a nil value.

So test presence with `v, ok := m[k]`, not with `m[k] == nil`.

### Why are keys that do not exist missing from `GetMany`'s `BatchError`?

In a batch read, **not existing is not an error**: a key that does not exist is simply absent from the map, and `BatchError` lists only the keys that failed. A cache's batch read almost always has misses; if they counted as errors, `err` would almost never be nil and every caller would have to filter them out. Each key ends in exactly one of three outcomes:

| Outcome | `Get` | `GetMany` |
|---|---|---|
| found | `(value, nil)` | in the map |
| does not exist | `IsErrKeyNotFound(err)` | neither in the map nor in the `BatchError` |
| failed | any other error | in the `BatchError` |

See [ADR 0004](adr/0004-batch-result-shape.md) (Chinese).

### Why does `Get` still return `ErrKeyNotFound`? Isn't that inconsistent with `GetMany`?

Both report the same three outcomes, each in its own way. `Get` returns a single value, so it cannot say "does not exist" by leaving something out: the value itself may legitimately be nil, and when `T` is an int, the zero value cannot mean "absent" at all. It is the same in `database/sql`: `QueryRow` returns `ErrNoRows` when nothing matches, while `Query` just returns no rows.

## Singleflight and the double-check

### How is cache stampede prevented?

In two layers:

1. **Singleflight** (primary): concurrent misses of one key fetch it once; the other requests wait for that result. `WithFetchConcurrency(n)` allows n concurrent fetches of one key (default 1, full deduplication), trading some redundancy for throughput. See [design/singleflight.md](design/singleflight.md) (Chinese).
2. **Double-check** (supplementary): request B reads a miss before request A's fetch backfills, but claims the key only after A's fetch has finished; B reads the cache again instead of fetching again.

### Is the double-check worth it? Is it on by default?

By default it runs on demand (`DoubleCheckAuto`): it re-reads only when this Client wrote the key's stripe after the request read the cache. Measured:

- with a 1 ms backend read and a hot key, upstream calls drop from 30 to 19 (18 when always on);
- cold keys see almost no extra reads (1.005 reads per `Get`, against 2 when always on);
- with an in-memory backend it makes no difference.

Use `DoubleCheckEnabled` when you need to see values other processes wrote to a shared cache. See [ADR 0008](adr/0008-on-demand-double-check.md) and [research/2026-10-double-check.md](research/2026-10-double-check.md) (Chinese).

### What happens if the upstream panics or calls `runtime.Goexit`?

Every waiter of that fetch gets an error; none hangs:

- a panic gives `panic during upstream fetch: …`, and an ERROR log with the stack;
- `Goexit` gives `upstream fetch exited without returning (runtime.Goexit)`.

The latter differs from `x/sync/singleflight` (where waiters wait until their own ctx ends), on purpose. See [ADR 0006](adr/0006-goexit-publishes-an-error.md) (Chinese).

## Write order and the stripe lock

### What is the point of guaranteeing the write order? The TTL applies anyway.

The TTL bounds how long old data lives **when you do not write**. `Set`/`Del` is you asking for "the new value, now". The order guarantee makes sure that request is not quietly undone by a fetch that started earlier. Without it, after `Del` returns successfully, the old value can still be backfilled and live for the whole TTL, and a user can see data turn new and then old again. See [design/write-order.md](design/write-order.md#先说结论对使用者意味着什么) (Chinese).

### In layered caching, does a read write the next layer through `Set`/`Del`, taking write locks?

No. A read's backfill writes this layer's backend directly, holding only a read lock; on a miss, L1 calls L2's `Get`. Write locks are taken only when your code calls `Set`/`Del`.

### How far does one slow operation reach?

- A slow `Set`/`Del` holds only its own stripe (one of 4096);
- a large batch backfill holds the stripes of all its keys while it writes (about 22% for 1,000 keys), and writes to those stripes queue behind it;
- reads never wait.

For batches well over 1,000 keys, `WithGetManyChunkSize` makes each chunk backfill on its own and hold only its own stripes; chunks run concurrently, so to limit the stripes held at once, also lower `WithGetManyFetchConcurrency`. A smaller backend `ChunkSize` does not help. See [design/write-order.md](design/write-order.md#慢操作会拖慢多大范围) (Chinese).

### With several instances deployed, is the write order still guaranteed?

No. The stripe lock works within one process only:

- other instances' memory layers do not see your write and keep serving the old value until it expires;
- another instance can backfill an old value into the shared Redis.

TTLs are the backstop for now, so set both the memory layer's and the shared layer's TTL to how long you can accept old data. Redis leases and invalidation broadcasts are in the [to-do list](todo.md) (Chinese). See [design/consistency.md](design/consistency.md) (Chinese).

## Batch reads

### Does `GetMany` return keys one by one, or all together?

All together. Fresh values, stale values (with serve-stale) and fetched values all go into one result, returned at once. Stale keys are refreshed separately in the background.

### How long does a concurrent `Get` wait for a key that `GetMany` is fetching?

- With a batch upstream, the `Get` waits for that whole batch call (or, with chunking, that chunk) to return.
- Without one, a key is claimed only when its turn comes, so the `Get` is not held up by `GetMany`'s queue.

### What is the difference between `WithFetchConcurrency` and `WithGetManyFetchConcurrency`?

- The former bounds how many fetches **one key** can have at once;
- the latter bounds how many requests **one `GetMany`** sends upstream at once (per-key `Get`s, or chunk calls of a batch upstream).

## Backends

### What does GORMCache need on MySQL?

- **Creating the table** needs MySQL 8.0.17 or later: `Migrate` uses `utf8mb4_0900_bin`, so keys compare case and trailing spaces exactly, and it checks the version before creating the table.
- Existing tables are left as they are and never return another key's value; keys that differ only in case take a shared row over from each other. The statement to convert an old table is in [design/backends.md](design/backends.md#key-必须精确比较) (Chinese).

### Do concurrent writes to GORMCache from several instances deadlock?

They rarely fail: writes lock rows in byte order, and the occasional deadlock (MySQL's gap locks) is retried automatically (at most 5 attempts). When the attempts run out, the error is returned; a failed backfill is only logged at WARN and the next read fetches again. Old MySQL `*_ci` tables do not lock in byte order and rely on the retry alone. Inside your own transaction (`WithGORMTx`) nothing is retried, since the deadlock has rolled the whole transaction back. See [ADR 0010](adr/0010-gorm-deadlock-ordering-and-retry.md) (Chinese).
