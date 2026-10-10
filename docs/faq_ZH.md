# 常见问题

[English](faq.md) | 中文

每个问题先给结论，再给依据。设计细节以 [design.md](design.md) 及其分篇为准。

## 使用

### 什么时候用 `Entry[T]`，什么时候自定义新鲜度判断？

只按时间判断过期时，用 `Entry[T]` 配合 `EntryWithTTL(freshTTL, staleTTL)`。需要业务相关的判断时（比如检查值里的 `version` 字段），用 `WithStale` 传入自己的判断函数。见 [design/read-path.md](design/read-path.md#新鲜度)。

### 新鲜期和陈旧期有什么区别？

新鲜期内直接返回；新鲜期之后的**额外**一段时间是陈旧期，开启 `WithServeStale` 时，这段时间里的读取会先拿到旧值，同时后台刷新。条目的总寿命是「新鲜期 + 陈旧期」。后端自己的 TTL 要不小于这个总寿命，否则陈旧期还没到，条目就已经没了。

### 是不是所有数据库查询都该缓存？

不是。缓存访问频繁、相对稳定的数据。不适合缓存的：

- 变化非常频繁的数据（要求不到 1 秒就更新）；
- 基数很高、按用户区分的数据；
- 在内存里存不划算的大对象。

### 改了数据源之后要做什么？需要先拿分片锁吗？

不需要，也拿不到，分片锁是库内部的。只要遵守一个顺序：**先改数据源，再通过同一个 Client 调用 `Set` 或 `Del`**。顺序反过来（先 `Del` 再改数据源），中间回源的请求会读到旧值，并合法地把它回填进缓存。见 [design/write-order.md](design/write-order.md#使用方式改数据源时要做什么)。

### 缓存的值是 nil，怎么和「不存在」区分？

缓存一个 nil（比如空指针）也算命中：

- `Get` 返回 `(nil, nil)`，不会返回 `ErrKeyNotFound`；
- `GetMany` 的 map 里有这个 key，值是 nil。

所以判断 key 在不在，要用 `v, ok := m[k]`，不能用 `m[k] == nil`。

### 为什么 `GetMany` 的 `BatchError` 里没有「不存在」的 key？

批量读里，**不存在不算错误**：不存在的 key 就是不出现在 map 里，`BatchError` 只列真正失败的 key。缓存的批量读几乎每次都有未命中，如果把不存在也算作错误，`err` 就几乎总是非 nil，调用方每次都得过滤。每个 key 必然是三种结果之一：

| 结果 | `Get` | `GetMany` |
|---|---|---|
| 找到了 | `(value, nil)` | 在 map 里 |
| 不存在 | `IsErrKeyNotFound(err)` | 既不在 map 里，也不在 `BatchError` 里 |
| 失败了 | 其他 error | 在 `BatchError` 里 |

见 [ADR 0004](adr/0004-batch-result-shape.md)。

### `Get` 为什么还要返回 `ErrKeyNotFound`，和 `GetMany` 不一致？

两者表达的是同一套三种结果，只是方式不同。`Get` 只返回一个值，没法用「缺席」表示不存在：值本身可能就是合法的 nil；`T` 是 int 时，零值更无法表示缺席。这和 `database/sql` 一样：`QueryRow` 查不到时返回 `ErrNoRows`，`Query` 查不到时只是没有行。

## 合并回源与二次检查

### 缓存击穿是怎么防的？

两层：

1. **合并回源**（主要）：同一个 key 的并发未命中只回源一次，其他请求等它的结果。`WithFetchConcurrency(n)` 允许同一个 key 同时有 n 个回源（默认 1，即完全合并），用一些冗余换取更高的吞吐。见 [design/singleflight.md](design/singleflight.md)。
2. **二次检查**（补充）：请求 B 在请求 A 回填之前读到未命中，却在 A 的回源结束之后才认领，B 会再读一次缓存，而不是再回源一次。

### 二次检查值得开吗？默认开吗？

默认按需开启（`DoubleCheckAuto`）：只有在请求读缓存之后、本 Client 往这个 key 所在的分片写过东西时，才再读一次。实测：

- 后端读取 1ms、热点 key 时，上游调用从 30 次降到 19 次（总是开启是 18 次）；
- 冷 key 上几乎没有额外读取（每次 `Get` 读 1.005 次，总是开启是 2 次）；
- 后端是内存时，开不开没有区别。

需要看到其他进程写进共享缓存的值时，用 `DoubleCheckEnabled`。见 [ADR 0008](adr/0008-on-demand-double-check.md) 和 [research/2026-10-double-check.md](research/2026-10-double-check.md)。

### 上游 panic 或调用 `runtime.Goexit` 时会怎样？

这次回源的所有等待方都会收到错误，不会挂住：

- panic 时是 `panic during upstream fetch: …`，并记一条带调用栈的 ERROR 日志；
- Goexit 时是 `upstream fetch exited without returning (runtime.Goexit)`。

后者和 `x/sync/singleflight` 不同（x/sync 会让等待方一直等到自己的 ctx 结束），是有意的，见 [ADR 0006](adr/0006-goexit-publishes-an-error.md)。

## 写入顺序与分片锁

### 保证写入顺序有什么用？反正都要承受 TTL。

TTL 是**不主动写入**时旧数据的存活上限。`Set`/`Del` 是你主动要求「现在就变成新的」。顺序保证的是，这个要求不会被一个更早开始的回源悄悄撤销。没有它，`Del` 返回成功后，旧值仍可能被回填进去，并存活整个 TTL；用户还可能看到数据先变新、再变回旧的。见 [design/write-order.md](design/write-order.md#先说结论对使用者意味着什么)。

### 分层缓存里，读取会不会通过 `Set`/`Del` 去写下一层、从而加写锁？

不会。读取时的回填直接写本层的后端，只拿读锁；L1 未命中时调用的是 L2 的 `Get`。只有业务代码主动调用 `Set`/`Del` 时才会加写锁。

### 一个慢操作会拖慢多大范围？

- 慢的 `Set`/`Del` 只占它自己的 1 个分片（共 4096 个）；
- 一次大批量回填会在回填期间占住它所有 key 的分片（1000 个 key 约 22%），这些分片上的写入要排队；
- 读永远不等。

批量远超 1000 个 key 时，用 `WithGetManyChunkSize` 让每段各自回填、只占自己的分片；各段是并发的，要限制同时占住的分片，还要调小 `WithGetManyFetchConcurrency`。调小后端的 `ChunkSize` 没有这个效果。见 [design/write-order.md](design/write-order.md#慢操作会拖慢多大范围)。

### 多个实例部署时，还保证写入顺序吗？

不保证。分片锁只在一个进程内有效：

- 其他实例的内存层看不到你的写入，会一直返回旧值，直到过期；
- 另一个实例可能把旧值回填进共享的 Redis。

目前靠 TTL 兜底，所以内存层和共享层的 TTL 都要按「能接受旧数据存活多久」来设。Redis 租约和失效广播在 [todo](todo.md) 里。见 [design/consistency.md](design/consistency.md)。

## 批量读

### `GetMany` 的结果是一个一个返回的，还是一起返回？

一起返回。新鲜的值、陈旧的值（开启返回陈旧值时）、回源拿到的值都放进同一份结果，最后一次性返回。陈旧的 key 会在后台另外刷新。

### 并发的 `Get` 碰上 `GetMany` 正在回源的 key，要等多久？

- 上游支持批量时，`Get` 要等这一次批量调用（设置了分段时是这一段）整体返回。
- 上游不支持批量时，key 轮到它才被认领，所以 `Get` 不会被 `GetMany` 的排队拖住。

### `WithFetchConcurrency` 和 `WithGetManyFetchConcurrency` 有什么区别？

- 前者限制**同一个 key** 最多同时有几个回源；
- 后者限制**一次 `GetMany`** 最多同时向上游发出几个请求（逐 key 的 `Get`，或分段的批量调用）。

## 后端

### MySQL 上用 GORMCache 有什么要求？

- **新建表**需要 MySQL 8.0.17 及以上：`Migrate` 会用 `utf8mb4_0900_bin`，让 key 精确比较大小写和末尾空格，建表前会检查版本。
- **已经存在的表**，`key` 列必须是 `utf8mb4_0900_bin`，否则 `Migrate` 会报错，错误里带转换语句（`ALTER TABLE <表> CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`）。MySQL 默认配置下建的 `*_ci` 表要先转换。见 [design/backends.md](design/backends.md#key-必须精确比较)。

### 多个实例同时写 GORMCache 会死锁吗？

极少失败：写入统一按字节序加锁，偶发的死锁（MySQL 的间隙锁）会自动重试（最多执行 5 次）。重试用尽仍会报错；回填失败只记 WARN 日志，下次读取再回源。在你自己的事务里（`WithGORMTx`）不会重试，因为死锁已经回滚了整个事务。见 [ADR 0010](adr/0010-gorm-deadlock-ordering-and-retry.md)。

### GORMCache 对数据库的隔离级别有要求吗？

只支持、也只测过各数据库的默认隔离级别（MySQL 的 REPEATABLE READ、PostgreSQL 的 READ COMMITTED、SQLite 的 SERIALIZABLE）。cachex 不设置隔离级别，每段写入都是一条自动提交的语句。不要改掉缓存表所用连接的默认隔离级别。

### 表的 key 列已经是 `utf8mb4_0900_bin`，还需要死锁重试吗？

需要。`utf8mb4_0900_bin` 让加锁顺序和字节序一致，消除了大部分死锁；剩下的来自 MySQL 默认隔离级别下的间隙锁，排序消除不了，要靠重试。PostgreSQL 上统一顺序之后实测没有死锁。重试只在语句已经因为死锁失败时才发生，没有死锁时不产生任何开销。见 [design/backends.md](design/backends.md#隔离级别)。
