# 后端

各个内置后端的实现要点和已知限制。所有后端都实现了 `Cache[T]`：未命中时 `Get` 返回 `*ErrKeyNotFound`（可以再包一层）。实现了 `BatchCache[T]` 的后端，`GetMany`/`SetMany`/`DelMany` 遵守 [batch.md](batch.md#批量上游要遵守的约定) 里的约定。

| 后端 | 存在哪 | 批量 | 自己的 TTL | 适合 |
|---|---|---|---|---|
| `RistrettoCache` | 进程内存（dgraph-io/ristretto） | ✅ | ✅ | 内存层 |
| `SyncMap` | 进程内存（`sync.Map`） | ✅ | ❌ | 测试、小数据量 |
| `BigCache` | 进程内存（allegro/bigcache），只存 `[]byte` | ❌ | 由 bigcache 配置 | 大量小值、在意 GC |
| `RedisCache` | Redis（单机或 Cluster） | ✅ | ✅ | 共享层 |
| `GORMCache` | 数据库表（MySQL 8.0.17+、PostgreSQL、SQLite） | ✅ | ❌（清理见 [todo](../todo.md)） | 持久化的共享层 |
| `Transform` | 包装另一个后端，做类型转换 | ❌ | 取决于被包装的后端 | 在不同值类型之间复用同一个后端 |

没有实现批量接口的后端（`BigCache`、`Transform`、自定义后端），`GetMany` 会逐个 key 读写。注意：用 `Transform` 包一层 `RedisCache`，`GetMany` 就会变成每个 key 一次往返。

## RistrettoCache

- `Set` 之后调用 `Wait()`，等缓冲区里的写入真正生效，保证 `Set` 返回后立即 `Get` 能读到。`SetMany`/`DelMany` 写完全部 key 后只 `Wait()` 一次。
- Ristretto 的准入策略（W-TinyLFU）可能**静默丢弃**一次写入，被丢弃时不算错误：这本来就是它防止低价值条目污染缓存的手段，丢了也只是下次未命中。
- 每个条目的代价（cost）固定为 1，容量上限就是条目数。

## SyncMap

`sync.Map` 的简单包装，没有过期、没有容量限制。主要用于测试，或者作为确定不会无限增长的小数据量内存层。

## RedisCache

### 编码

| 值的类型 | 怎么存 |
|---|---|
| `string`、`[]byte` | 原样存 |
| 实现了 `encoding.BinaryMarshaler`（且 `*T` 实现了 `BinaryUnmarshaler`） | 二进制 |
| 其他 | JSON |

读取时用 `StringCmd.Bytes()` 拿回复。它和回复共用内存，不再复制一次。`T = []byte` 时，返回的切片也直接共用这块内存；这个命令对象用完就丢弃，没有别人持有它，所以是安全的。

### 批量：pipeline

- `GetMany`/`SetMany`/`DelMany` 用 `GET`/`SET`/`DEL` 的 pipeline，而不是 `MGET`。这样在 Redis Cluster 上，一批 key 即使分布在不同的 slot 也能用。
- 大批量按 `RedisCacheConfig.ChunkSize`（默认 1000）切成多个 pipeline，**依次**发送。Redis 服务端是单线程的，并发发送多个 pipeline 收益很小。
- **每条命令的错误各自判断**：pipeline 的 `Exec` 只返回第一个失败命令的错误。Redis 回复的错误（例如对一个 list 执行 `GET` 返回 `WRONGTYPE`）已经记在各自的命令上；但**连接层面的失败**（连不上、连接中断）发生时，没发出去的命令自身是没有错误的。所以 `Exec` 返回的错误如果不是 Redis 的回复错误，就把它设到每一条还没有错误的命令上。否则 `GET` 会被当成读到了空值，Redis 故障时所有 key 都会被当成「命中空字符串」。
- 写入尽力而为：失败的 key（编码失败、命令失败）列在 `*BatchError` 里，其余照常写入。

## GORMCache

### 表结构

```go
type cacheEntry struct {
    Key       string         `gorm:"not null;primaryKey;size:255"`
    Value     datatypes.JSON `gorm:"not null;type:json"`
    UpdatedAt time.Time      `gorm:"not null;index"`
}
```

值以 JSON 存储。`KeyPrefix` 会拼在 key 前面，同一张表可以给多个用途使用。

### key 必须精确比较

缓存 key 区分大小写，也区分末尾空格。不同数据库的默认行为不同：

| 数据库 | key 列默认怎么比较 | cachex 的处理 |
|---|---|---|
| PostgreSQL、SQLite | 逐字节 | 不需要额外处理 |
| MySQL | 默认排序规则（`utf8mb4_0900_ai_ci`）**不区分大小写和重音** | `Migrate` 建表时使用 `utf8mb4_0900_bin`。建表前检查版本：低于 8.0.17 的 MySQL 和 MariaDB 都没有这个排序规则，会返回写明版本号的错误。已经存在的表不会改动 |

即使 key 列不能精确比较（比如升级前建的老表），也**不会读到别的 key 的值**：

- `Get`/`GetMany` 只返回存储的 key 和请求的 key **完全相等**的那一行；
- upsert 时连同 key 列一起改写，所以一行共享的数据只属于最后写入它的那个 key。

代价是只差大小写的几个 key 会互相挤占同一行，表现为多几次未命中。把老表转成精确比较：`ALTER TABLE <表> CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`。决策过程见 [ADR 0009](../adr/0009-gorm-exact-keys-and-collation.md)。

### `key` 是 MySQL 的保留字

所有条件都用 GORM 的 `clause.Eq`、`clause.IN` 构造，由 GORM 给列名加引号。不能写成 `Where("key = ?")` 这样的原始 SQL 片段：SQLite 能接受，MySQL 会报语法错误。有一个单元测试会截获实际执行的 SQL，检查列名有没有加引号。

### 批量与死锁

- `GetMany`/`SetMany`/`DelMany` 按 `GORMCacheConfig.ChunkSize`（默认 1000，最大 10000，以免超出 SQLite 的 32766 个绑定参数上限）切成多条语句，依次执行，**每条语句独立提交**（在 `WithGORMTx` 传入的事务里时除外，那时都在调用方的事务里）。某一段失败时，这一段的 key 记进 `*BatchError`，然后继续执行后面的段。
- **统一加锁顺序**：
  - `SetMany` 插入前按 key 的字节序排序。否则多个实例同时回填有重叠的 key 时，会以相反的顺序锁行而死锁。
  - PostgreSQL 的 `DelMany` 先用 `SELECT … ORDER BY key COLLATE "C" FOR UPDATE` 按字节序锁好行，再删除。`DELETE` 是按索引顺序加锁的，而索引顺序跟着列的排序规则走（官方镜像默认是 `en_US.utf8`），和 `SetMany` 的字节序不一致。
  - MySQL 新表的 `utf8mb4_0900_bin` 按码点排序，和 UTF-8 的字节序一致，所以不需要额外处理。
- **死锁重试**：MySQL 的间隙锁仍可能偶发死锁，InnoDB 要求应用自行重试。所以 `Set`/`Del`/`SetMany`/`DelMany` 被数据库判为死锁牺牲者时会重试，最多执行 5 次（即重试 4 次），每次退避的时间带随机抖动，并尊重 ctx。识别方式：PostgreSQL 看 SQLSTATE `40P01`/`40001`；MySQL 匹配驱动的错误文本 `Error 1213 (40001)`，因为 go-sql-driver 的错误类型没有提供 SQLState 方法。
- **调用方自己的事务里不重试**：用 `WithGORMTx` 把事务放进 ctx 时，死锁已经让数据库回滚了整个事务，只重试一条语句是错的，所以直接把错误返回给调用方。

这几种做法的实测对比（包括锁表）见 [research/2026-10-gorm-deadlock.md](../research/2026-10-gorm-deadlock.md)，决策见 [ADR 0010](../adr/0010-gorm-deadlock-ordering-and-retry.md)。

### 事务

`WithGORMTx(ctx, tx)` 让 GORMCache 在调用方的事务里读写。但**回源时**，cachex 会把这个事务从 ctx 中去掉（见 [singleflight.md](singleflight.md#回源用的-ctx)）：一次回源服务的是所有等待方，回填不能写进某一个调用方的事务里。

### 已知限制

- 没有过期清理：表只增不减，需要自己定期删除旧行。清理任务在 [todo](../todo.md) 里。
- 值为 JSON 标量数字（例如 `T = float64`）时，SQLite 会把它存成数字类型，`datatypes.JSON` 读回来时扫描失败。结构体、字符串等类型不受影响。见 [todo](../todo.md)。

## BigCache

只存 `[]byte`，适合大量小值、希望减少 GC 压力的场景。要存其他类型，用 `Transform` 包一层。

## Transform

`Transform(cache, encode, decode)` 把一个 `Cache[A]` 包装成 `Cache[B]`，比如把 `RedisCache[[]byte]` 包成 `Cache[*Product]`。它没有实现批量接口。
