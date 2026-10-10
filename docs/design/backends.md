# 后端

后端是一层的存储：它只按 key 存取 `Entry[T]`，条目里的时间都由 `Cache` 算好。内置后端各在一个子包里，各自依赖自己的库（见 [ADR 0013](../adr/0013-batch-backends-in-subpackages.md)）。

| 子包 | 存在哪 | 怎么过期 | 适合 |
|---|---|---|---|
| `ottercachex` | 进程内存（otter v2），存值本身 | 每个条目按 `ExpiresAt` | 内存层 |
| `bigcachex` | 进程内存（BigCache），存编码后的字节 | BigCache 的 `LifeWindow` 和容量淘汰 | 内存层条目多到 GC 明显吃 CPU 时 |
| `rediscachex` | Redis（单机或 Cluster） | Redis 原生 TTL，按 `ExpiresAt` | 多实例共享的层 |
| `gormcachex` | 数据库表（MySQL 8.0.17+、PostgreSQL、SQLite） | 不清理，读时由 `Cache` 判断 | 持久化的共享层 |
| `cachextest` | 进程内存（`sync.Map`），没有上限 | 不清理 | 只用于测试 |

## 后端接口

```go
type Backend[T any] interface {
    Get(ctx, key) (Entry[T], bool, error)               // 没有：false，不是错误
    GetMany(ctx, keys) (map[string]Entry[T], error)     // 没有的 key 不在 map 里
    Set(ctx, key, Entry[T]) error
    SetMany(ctx, map[string]Entry[T]) error
    Del(ctx, key) error
    DelMany(ctx, keys) error
}

type SingleBackend[T any] interface { Get; Set; Del }  // 只有单 key 能力的存储
func Batched[T any](b SingleBackend[T]) Backend[T]     // 批量方法逐个 key 调用，尽力而为
```

- 未命中不用错误表示，`Cache` 也不拿错误做控制流。
- 批量方法遵守 [batch.md](batch.md#批量调用要遵守的约定) 的约定：部分失败返回原样的 `*BatchError`，其他错误表示整批失败。
- 后端可以从 `ExpiresAt` 起丢掉条目（`rediscachex`、`ottercachex` 原生过期），也可以留着：Cache 不会返回腐烂的条目。在那之前也可以因为容量淘汰而丢掉条目，那只是一次未命中。
- `cachextest.TestBackend(t, newBackend)` 是所有后端共用的契约测试：未命中不是错误、条目原样读回（包括不存在记录和各种字符串）、key 精确比较（大小写、末尾空格、重音）、批量操作和空批量。自写后端可以直接拿它来测。

## 编码：存字节的后端

`bigcachex`、`rediscachex`、`gormcachex` 存的是字节，值的编码由 `Codec[T]` 负责，条目的其余字段由 `cachex.EncodeEntry` / `DecodeEntry` 统一编码：

| 字节 | 内容 |
|---|---|
| 0 | 格式版本，目前是 1 |
| 1 | 标志位：最低位表示不存在记录，其余位必须为 0 |
| 2–25 | `CachedAt`、`FreshUntil`、`ExpiresAt`，各 8 字节大端的 Unix 纳秒，零值时间存 0 |
| 26 起 | 值的编码；不存在记录没有这一段 |

`DefaultCodec[T]`（后端配置里不给 `Codec` 时用它）：

| 值的类型 | 怎么存 |
|---|---|
| `string`、`[]byte` | 原样存 |
| `T` 实现了 `encoding.BinaryMarshaler`，且 `*T` 实现了 `BinaryUnmarshaler` | 二进制 |
| 其他 | JSON（`JSONCodec[T]`） |

**解码失败算未命中**：格式不对、标志位不对、值解不出来，后端都记一条 WARN 日志、删掉这个条目，当作没有。读取照常回源，结果覆盖坏数据。这样结构体发生不兼容的改动后，旧数据不会让读取一直报错。

但 JSON 解码很宽松：字段改了名，旧数据会照样解码成功，那个字段是零值。所以结构体有不兼容的改动时，换一个 `KeyPrefix`。

内存后端（`ottercachex`、`cachextest`）存的是值本身：`T` 是指针时，所有调用方拿到的是**同一个**对象，修改返回的值就是在修改缓存。存不会被修改的值，或者存副本。`bigcachex` 每次读都解码出新的值，没有这个问题。

## ottercachex

- 必须给出容量：`MaximumSize`（条目数），或 `MaximumWeight` 加 `Weigher`（比如按字节），两者正好选一个，否则 `New` 返回错误。内存层必须有上限。
- 每个条目在 `ExpiresAt` 过期（otter 的 `ExpiryCalculator`，按写入时的剩余时间计算，用的是真实时钟）。
- 写入同步可见，不需要等待缓冲区。otter 的淘汰在它自己的维护过程中进行，条目数可能短暂超过上限。
- 为什么用 otter 而不是 ristretto，见 [ADR 0014](../adr/0014-otter-for-the-memory-layer.md)。

## bigcachex

- 配置里传入一个已经建好的 `*bigcache.BigCache`。BigCache 所有条目共用一个寿命（`LifeWindow`），要设成不小于这一层最长的「新鲜期 + 陈旧期」，否则条目会提前消失。
- 过了 `ExpiresAt` 的条目留在 BigCache 里，直到它自己的淘汰清掉，但不会被返回。
- 值存成字节，GC 不用扫描它们；代价是每次读都要解码。
- 没有原生批量操作，`GetMany`/`SetMany`/`DelMany` 逐个 key 进行，尽力而为。

## rediscachex

- 每个条目一个字符串 key（`KeyPrefix + key`），用带 TTL 的 `SET` 写入，TTL 是 `ExpiresAt` 减去现在；已经过期的条目改为 `DEL`。
- 读取用 `StringCmd.Bytes()` 拿回复，它和回复共用内存，不再复制一次。`T = []byte` 时，返回的切片直接共用这块内存；这个命令对象用完就丢弃，没有别人持有它，所以是安全的。

### 批量：pipeline

- `GetMany`/`SetMany`/`DelMany` 用 `GET`/`SET`/`DEL` 的 pipeline，而不是 `MGET`。这样在 Redis Cluster 上，一批 key 即使分布在不同的 slot 也能用。
- 大批量按 `ChunkSize`（默认 1000）切成多个 pipeline，**依次**发送。Redis 服务端是单线程的，并发发送多个 pipeline 收益很小。
- **每条命令的错误各自判断**：pipeline 的 `Exec` 只返回第一个失败命令的错误。Redis 回复的错误（例如对一个 list 执行 `GET` 返回 `WRONGTYPE`）已经记在各自的命令上；但**连接层面的失败**（连不上、连接中断）发生时，没发出去的命令自身是没有错误的。所以 `Exec` 返回的错误如果不是 Redis 的回复错误，就把它设到每一条还没有错误的命令上（`execPipe`）。否则 Redis 故障时，所有 `GET` 都会被当成未命中。
- 写入尽力而为：失败的 key（编码失败、命令失败）列在 `*BatchError` 里，其余照常写入。

## gormcachex

### 表结构

```go
type row struct {
    Key       string    `gorm:"not null;primaryKey;size:255"`
    Value     []byte    `gorm:"not null"`       // EncodeEntry 的结果
    ExpiresAt time.Time `gorm:"not null;index"` // 给清理过期行用
    UpdatedAt time.Time `gorm:"not null"`
}
```

`KeyPrefix` 会拼在 key 前面，同一张表可以给多个用途使用；key 列最长 255 个字符，**包括前缀**，更长的 key 写不进去（回填失败只记 WARN，这个 key 就每次都未命中）。`Migrate` 建表。表不会自动清理，过了 `expires_at` 的行要自己定期删除（清理任务在 [todo](../todo.md) 里）。

### key 必须精确比较

缓存 key 区分大小写，也区分末尾空格。不同数据库的默认行为不同：

| 数据库 | key 列默认怎么比较 | gormcachex 的处理 |
|---|---|---|
| PostgreSQL、SQLite | 逐字节 | 不需要额外处理 |
| MySQL | 默认排序规则（`utf8mb4_0900_ai_ci`）**不区分大小写和重音** | `Migrate` 建表时使用 `utf8mb4_0900_bin`。建表前检查版本：低于 8.0.17 的 MySQL 和 MariaDB 都没有这个排序规则，会返回写明版本号的错误。表已经存在时，检查 `key` 列的排序规则，不是 `utf8mb4_0900_bin` 就返回错误，错误里带转换语句 |

`Migrate` 不替你改表。用 MySQL 默认配置建的表要自己转换：`ALTER TABLE <表> CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`。不调 `Migrate`、自己建表时，同样要用这个排序规则。

读写本身还多一道防线，万一碰上不能精确比较的表（比如没调 `Migrate`、自己建的表），也**不会读到别的 key 的值**：`Get`/`GetMany` 只返回存储的 key 和请求的 key **完全相等**的那一行；upsert 时连同 key 列一起改写，所以共用的一行只属于最后写入它的 key。代价是只差大小写的 key 会互相挤占同一行，表现为多几次未命中。决策过程见 [ADR 0009](../adr/0009-gorm-exact-keys-and-collation.md)。

### `key` 是 MySQL 的保留字

所有条件都用 GORM 的 `clause.Eq`、`clause.IN` 构造，由 GORM 给列名加引号。不能写成 `Where("key = ?")` 这样的原始 SQL 片段：SQLite 能接受，MySQL 会报语法错误。有一个单元测试会截获实际执行的 SQL，检查列名有没有加引号。

### 批量与死锁

- `GetMany`/`SetMany`/`DelMany` 按 `ChunkSize`（默认 1000，最大 8000：一行绑定 4 个参数，SQLite 一条语句最多 32766 个）切成多条语句，依次执行，**每条语句独立提交**（在 `WithTx` 传入的事务里时除外）。某一段失败时，这一段的 key 记进 `*BatchError`，然后继续执行后面的段。单个 key 的方法就是只有一个 key 的批量。
- 已经过期的条目不写入，改为删除。
- **统一加锁顺序**：
  - `SetMany` 插入前按 key 的字节序排序。否则多个实例同时回填有重叠的 key 时，会以相反的顺序锁行而死锁。
  - `DelMany` 同样先按 key 排序；PostgreSQL 上再用 `SELECT … ORDER BY key COLLATE "C" FOR UPDATE` 按字节序锁好行，再删除。`DELETE` 是按扫描顺序加锁的，而扫描顺序跟着列的排序规则走（官方镜像默认是 `en_US.utf8`），和 `SetMany` 的字节序不一致。
  - MySQL 的 `utf8mb4_0900_bin` 按码点排序，和 UTF-8 的字节序一致，所以不需要额外处理。
- **死锁重试**：MySQL 的间隙锁仍可能偶发死锁，InnoDB 要求应用自行重试。所以每条写语句被数据库判为死锁牺牲者时会重试，最多执行 5 次（即重试 4 次），每次退避的时间带随机抖动，并尊重 ctx。识别方式：PostgreSQL 看 SQLSTATE `40P01`/`40001`；MySQL 匹配驱动的错误文本 `Error 1213 (40001)`，因为 go-sql-driver 的错误类型没有提供 SQLState 方法。
- **调用方自己的事务里不重试**：死锁已经让数据库回滚了整个事务，只重试一条语句是错的，所以直接把错误返回给调用方。

### 隔离级别

只支持、也只测过各数据库的**默认**隔离级别：MySQL InnoDB 的 REPEATABLE READ、PostgreSQL 的 READ COMMITTED、SQLite 的 SERIALIZABLE。gormcachex 不设置隔离级别，每一段写入都是一条自动提交的语句，用的是连接的默认值；不要把缓存表所用连接的默认隔离级别改掉。

- MySQL 上剩下的偶发死锁正是 REPEATABLE READ 的间隙锁造成的，靠重试兜住。改用 READ COMMITTED 实测死锁反而更多。
- 统一加锁顺序之后，PostgreSQL 在默认级别下实测没有死锁，重试只是保险。

这几种做法的实测对比（包括锁表）见 [research/2026-10-gorm-deadlock.md](../research/2026-10-gorm-deadlock.md)，决策见 [ADR 0010](../adr/0010-gorm-deadlock-ordering-and-retry.md)。

### 事务

`WithTx(ctx, tx)` 让 gormcachex 用这个 ctx 的读写都在调用方的事务里进行，`TxFrom(ctx)` 取回它。但 ctx 带着共享标记时（`cachex.IsShared(ctx)`，即回源、回填、写入失败后的失效），**不用**这个事务：一次回源服务的是所有等待方，回填不能写进某一个调用方的事务里（见 [ADR 0007](../adr/0007-detached-fetch-context.md)）。

所以 `Cache.Set(WithTx(ctx, tx), …)` 写这一层是在事务里的；事务回滚后，这一层的写入也没了，而上面的层（比如内存层）仍是新值，直到它过期。需要两者一致时，在事务提交之后再调用 `Set`/`Del`。

## cachextest

- `Map[T]`：基于 `sync.Map`、没有容量上限、从不清理的 `Backend`，只用于测试。
- `Clock`：手动时钟，`Now` 传给 `cachex.WithNow`，`Advance` 拨动它。
- `TestBackend`：上面说的契约测试。
