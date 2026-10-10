# 常见问题

[English](faq.md) | 中文

每个问题先给结论，再给依据。设计细节以 [design.md](design.md) 及其分篇为准。

## 使用

### 有些值本身会过期，比如 token，怎么避免返回得太久？

用 `WithMaxAge(func(T) time.Duration)`。它在回源拿到值时调用一次，限制这个值在每一层最多保存多久；返回零或负数表示不缓存这个值。它接收的是函数而不是 `T` 上的方法，所以别的包里的类型也能用。「版本 N 之前的全部作废」这种需求，改为把版本号放进 key。

### 新鲜期和陈旧期有什么区别？

两者都按层用 `TTL(fresh, stale)` 设置，从数据源回答的时间起算，而不是从写进这一层的时间。新鲜期内条目直接返回。陈旧期是**额外**的一段：这段时间里的读取立即拿到条目，同时后台刷新会重新回源。过了「新鲜期 + 陈旧期」，条目就腐烂了，不再返回。`Jitter` 把每个条目的新鲜期随机缩短（默认最多 10%），让同时写入的条目不在同一时刻过期。后端不需要自己的 TTL：每个条目腐烂时，后端就让它过期。

### 是不是所有数据库查询都该缓存？

不是。缓存访问频繁、相对稳定的数据。不适合缓存的：

- 变化非常频繁的数据（要求不到 1 秒就更新）；
- 基数很高、按用户区分的数据；
- 在内存里存不划算的大对象。

### 改了数据源之后要做什么？需要先拿分片锁吗？

不需要，也拿不到，分片锁是库内部的。只要遵守一个顺序：**先改数据源，再通过同一个 `Cache` 调用 `Set` 或 `Del`**。顺序反过来（先 `Del` 再改数据源），中间回源的请求会读到旧值，并合法地把它回填进缓存。见 [design/write-order.md](design/write-order.md)。

### 缓存的值是 nil，怎么和「不存在」区分？

缓存一个 nil（比如空指针）也算命中：

- `Get` 返回 `(nil, nil)`，不会返回 `ErrNotFound`；
- `GetMany` 的 map 里有这个 key，值是 nil。

所以判断 key 在不在，要用 `v, ok := m[k]`，不能用 `m[k] == nil`。

### 为什么 `GetMany` 的 `BatchError` 里没有「不存在」的 key？

批量读里，**不存在不算错误**：不存在的 key 就是不出现在 map 里，`BatchError` 只列真正失败的 key。缓存的批量读几乎每次都有未命中，如果把不存在也算作错误，`err` 就几乎总是非 nil，调用方每次都得过滤。每个 key 必然是三种结果之一：

| 结果 | `Get` | `GetMany` |
|---|---|---|
| 找到了 | `(value, nil)` | 在 map 里 |
| 不存在 | `errors.Is(err, ErrNotFound)` | 既不在 map 里，也不在 `BatchError` 里 |
| 失败了 | 其他 error | 在 `BatchError` 里 |

见 [ADR 0004](adr/0004-batch-result-shape.md)。

### `Get` 为什么还要返回 `ErrNotFound`，和 `GetMany` 不一致？

两者表达的是同一套三种结果，只是方式不同。`Get` 只返回一个值，没法用「缺席」表示不存在：值本身可能就是合法的 nil；`T` 是 int 时，零值更无法表示缺席。这和 `database/sql` 一样：`QueryRow` 查不到时返回 `ErrNoRows`，`Query` 查不到时只是没有行。

### 需要调用 `Close` 吗？

在关闭服务时调用，并且在关闭后端之前。它会等正在进行的回源和后台刷新结束（它们的回填还要写后端），之后不再启动刷新。它不会关闭后端。一个从不关闭的 `Cache`，在最后一次回源结束后也不会泄漏任何东西。

### 升级之后，v1 缓存的数据会怎样？

v2 读不了：存字节的后端把解不出来的数据当作未命中，删掉它并记一条警告，所以最初的读取会回源。换一个 `KeyPrefix`（`gormcachex` 用一张新表）可以跳过这一步。见 [MIGRATION_ZH.md](../MIGRATION_ZH.md)。

## 合并回源与二次检查

### 缓存击穿是怎么防的？

两层：

1. **合并回源**（主要）：同一个 key 的并发未命中，在所有层之间只回源一次，其他请求等它的结果。`WithFetchesPerKey(n)` 允许同一个 key 同时有 n 个回源（默认 1），用下层多一些负载换取更高的吞吐。见 [design/singleflight.md](design/singleflight.md)。
2. **二次检查**（补充）：请求 B 在请求 A 回填之前读到未命中，却在 A 的回源结束之后才认领，B 会再读一次第一层，而不是再回源一次。

### 二次检查值得开吗？默认开吗？

默认按需开启（`DoubleCheckAuto`）：只有在请求读第一层之后、本 `Cache` 往这个 key 所在的分片写过东西时，才再读一次。实测：

- 第一层读取 1ms、热点 key 时，数据源调用从 30 次降到 19 次（总是开启是 18 次）；
- 冷 key 上几乎没有额外读取（每次 `Get` 读 1.005 次，总是开启是 2 次）；
- 第一层是内存时，开不开没有区别。

需要看到其他进程写进共享第一层的值时，用 `DoubleCheckEnabled`。见 [ADR 0008](adr/0008-on-demand-double-check.md) 和 [research/2026-10-double-check.md](research/2026-10-double-check.md)。

### 数据源 panic 或调用 `runtime.Goexit` 时会怎样？

这次回源的所有等待方都会收到错误，不会挂住：

- panic 时是 `cachex: panic during fetch: …`，并记一条带调用栈的 ERROR 日志；
- Goexit 时是 `cachex: fetch exited without returning (runtime.Goexit)`。

后者和 `x/sync/singleflight` 不同（x/sync 会让等待方一直等到自己的 ctx 结束），是有意的，见 [ADR 0006](adr/0006-goexit-publishes-an-error.md)。

## 写入顺序与分片锁

### 保证写入顺序有什么用？反正都要承受 TTL。

TTL 是**不主动写入**时旧数据的存活上限。`Set`/`Del` 是你主动要求「现在就变成新的」。顺序保证的是，这个要求不会被一个更早开始的回源悄悄撤销。没有它，`Del` 返回成功后，旧值仍可能被回填进去，并存活整个 TTL；用户还可能看到数据先变新、再变回旧的。见 [design/write-order.md](design/write-order.md)。

### 读取会加写锁吗？

不会。读取的回填只以读的方式持有分片，分片被写入占着时就跳过回填（多一次未命中）。只有业务代码调用 `Set`、`Del`、`SetMany`、`DelMany` 时才加写锁。读永远不等写。

### 一个慢操作会拖慢多大范围？

- 慢的 `Set`/`Del` 只占它自己的 1 个分片（共 4096 个）；
- 一次大批量回填会在回填期间占住它所有 key 的分片（1000 个 key 约 22%），这些分片上的写入要排队；
- 读永远不等。

来自 `BatchSource` 的批量远超 1000 个 key 时，用 `WithGetManyChunkSize` 让每段各自回填、只占自己的分片；各段是并发的，要限制同时占住的分片，还要调小 `WithGetManyConcurrency`。调小后端的 `ChunkSize` 没有这个效果。见 [design/write-order.md](design/write-order.md)。

### 多个实例部署时，还保证写入顺序吗？

不保证。分片锁只在一个进程内有效：

- 其他实例的内存层看不到你的写入，会一直返回旧值，直到它腐烂；
- 另一个实例可能把旧值回填进共享的 Redis。

目前靠 TTL 兜底，所以每一层的 TTL 都要按「能接受旧数据存活多久」来设。Redis 租约和失效广播在 [todo](todo.md) 里。见 [design/consistency.md](design/consistency.md)。

## 批量读

### `GetMany` 的结果是一个一个返回的，还是一起返回？

一起返回。新鲜的值、陈旧的值、回源拿到的值都放进同一份结果，最后一次性返回。陈旧的 key 会在后台另外刷新。

### 并发的 `Get` 碰上 `GetMany` 正在回源的 key，要等多久？

`GetMany` 一开始就认领它要回源的全部 key，所以这些 key 的 `Get` 要等这个 key 的结果：

- 数据源是 `BatchSource` 时，等带着这个 key 的那次调用（设置了 `WithGetManyChunkSize` 时是那一段）返回；
- 不是时，等它在每次最多 `WithGetManyConcurrency` 个数据源调用里排到。

用在大批量 `GetMany` 里的数据源，请实现 `BatchSource`。

### `WithFetchesPerKey` 和 `WithGetManyConcurrency` 有什么区别？

- 前者限制**同一个 key** 最多同时有几个回源；
- 后者限制**一次 `GetMany`** 最多同时向数据源发出几个请求（普通数据源的 `Get` 调用，或 `BatchSource` 的分段调用）。

## 后端

### 为什么 `ottercachex` 必须设 `MaximumSize`？

没有上限的内存层会随着读过的每个 key 一直增长，直到进程内存耗尽。所以 `ottercachex` 拒绝没有 `MaximumSize`（条目数）、也没有 `MaximumWeight` 加 `Weigher`（比如字节数）的配置。满了之后，otter 淘汰最不可能再被读到的条目。

### 什么时候用 `bigcachex` 而不是 `ottercachex`？

内存层有数百万条目、GC 在 CPU profile 里显眼的时候。GC 要扫描每一个存活的指针，数百万个缓存的结构体每一轮都要它花工夫；`bigcachex` 存的是编码后的字节，GC 不用扫。代价是每次读取都要解码，`ottercachex` 没有这一步。条目在几十万以下时，用 `ottercachex`。见 [BENCHMARK_ZH.md](../BENCHMARK_ZH.md)。

### MySQL 上用 `gormcachex` 有什么要求？

- **MySQL 8.0.17 及以上**，不支持 MariaDB：`Migrate` 建表时 key 列用 `utf8mb4_0900_bin`，让 key 精确比较大小写和末尾空格，建表前会检查版本。
- **已经存在的表**，`key` 列必须是 `utf8mb4_0900_bin`，否则 `Migrate` 会报错，错误里带转换语句（`ALTER TABLE <表> CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`）。见 [design/backends.md](design/backends.md)。

### 多个实例同时写 `gormcachex` 会死锁吗？

极少失败：写入统一按字节序加锁，偶发的死锁（MySQL 的间隙锁）会自动重试（最多执行 5 次）。重试用尽仍会报错；回填失败只记 WARN 日志，下次读取再回源。在你自己的事务里（`gormcachex.WithTx`）不会重试，因为死锁已经回滚了整个事务。见 [ADR 0010](adr/0010-gorm-deadlock-ordering-and-retry.md)。

### `gormcachex` 对数据库的隔离级别有要求吗？

只支持、也只测过各数据库的默认隔离级别（MySQL 的 REPEATABLE READ、PostgreSQL 的 READ COMMITTED、SQLite 的 SERIALIZABLE）。cachex 不设置隔离级别，每段写入都是一条自动提交的语句。不要改掉缓存表所用连接的默认隔离级别。

### 表的 key 列已经是 `utf8mb4_0900_bin`，还需要死锁重试吗？

需要。`utf8mb4_0900_bin` 让加锁顺序和字节序一致，消除了大部分死锁；剩下的来自 MySQL 默认隔离级别下的间隙锁，排序消除不了，要靠重试。PostgreSQL 上统一顺序之后实测没有死锁。重试只在语句已经因为死锁失败时才发生，没有死锁时不产生任何开销。见 [design/backends.md](design/backends.md)。
