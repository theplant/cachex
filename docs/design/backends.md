# 后端

后端是一层的存储：它只按 key 存取 `Entry[T]`，条目里的时间都由 `Cache` 算好。存字节的后端只需实现更简单的 `ByteBackend`，由核心包的 `cachex.Encoded` 包成后端。内置后端各在一个子包里，各自依赖自己的库（见 [ADR 0013](../adr/0013-batch-backends-in-subpackages.md)、[ADR 0015](../adr/0015-byte-backends-through-encoded.md)）。

| 子包 | 存在哪 | 怎么过期 | 适合 |
|---|---|---|---|
| `ottercachex` | 进程内存（otter v2），存值本身 | 每个条目按 `ExpiresAt` | 内存层 |
| `bigcachex` | 进程内存（BigCache），经 `Encoded` 存编码后的字节 | BigCache 的 `LifeWindow` 和容量淘汰 | 内存层条目多到 GC 明显吃 CPU 时 |
| `rediscachex` | Redis（单机或 Cluster），经 `Encoded` 存字节 | Redis 原生 TTL，按 `ExpiresAt` | 多实例共享的层 |
| `gormcachex` | 数据库表（MySQL 8.0.17+、PostgreSQL、SQLite），经 `Encoded` 存字节 | 不清理，读时由 `Cache` 判断 | 持久化的共享层 |
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

type ByteBackend interface {                            // 只存字节的存储
    Get(ctx, key) ([]byte, bool, error)
    GetMany(ctx, keys) (map[string][]byte, error)
    Set(ctx, key, data []byte, expiresAt time.Time) error
    SetMany(ctx, map[string]ByteEntry) error            // ByteEntry{Data, ExpiresAt}
    Del(ctx, key) error
    DelMany(ctx, keys) error
}
func Encoded[T any](b ByteBackend, codec Codec[T], logger *slog.Logger) Backend[T]
```

- 未命中不用错误表示，`Cache` 也不拿错误做控制流。
- 批量方法遵守 [batch.md](batch.md#批量调用要遵守的约定) 的约定：部分失败返回原样的 `*BatchError`，其他错误表示整批失败。
- 后端可以从 `ExpiresAt` 起丢掉条目（`rediscachex`、`ottercachex` 原生过期），也可以留着：Cache 不会返回腐烂的条目。在那之前也可以因为容量淘汰而丢掉条目，那只是一次未命中。
- 单 key 的方法返回普通的 error；`*BatchError` 只出现在批量方法里（`NewBatchError(errs)` 构造它，`errs` 为空时返回 nil）。
- 契约测试见下文 [cachextest](#cachextest)，每个内置后端都要过。

## 字节后端与 Encoded

`bigcachex`、`rediscachex`、`gormcachex` 存的是字节。它们各自只实现一个私有的 `ByteBackend`，`New` 返回 `cachex.Encoded` 包好的后端（`gormcachex` 外面再包一层 `*Backend[T]`，加上 `Migrate`）；条目怎么编码、解不出来怎么办、已经腐烂的条目怎么处理，都只在 `Encoded` 里写一次（见 [ADR 0015](../adr/0015-byte-backends-through-encoded.md)）。自写的字节存储也这样做：实现 `ByteBackend`，再用 `Encoded` 包装。

### 编码

值的编码由 `Codec[T]` 负责，条目的其余字段由 `Encoded` 内部统一编码（编解码函数不公开，后端碰不到）：

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

### 解码失败算未命中

格式不对、标志位不对、值解不出来，`Encoded` 都记一条 WARN `cachex: dropped an entry that does not decode`、删掉这个 key，报告没有。读取照常回源，结果覆盖坏数据。这样结构体发生不兼容的改动后，旧数据不会让读取一直报错。

这次删除不经过写入顺序守卫，可能删掉并发 `Set` 刚写进来的新值。代价只是多一次未命中，不会让旧值留下来，所以不值得为它加锁。

但 JSON 解码很宽松：字段改了名，旧数据会照样解码成功，那个字段是零值。所以结构体有不兼容的改动时，换一个 `KeyPrefix`。

### 过期按真实时钟

`Encoded` 写入时，`ExpiresAt` 不晚于真实时钟（`time.Now`）的条目不写，改为删除这个 key；批量写里这样的条目合成一次 `DelMany`。判断用真实时钟而不是 `Cache` 的 `WithNow`，因为后端的过期都按真实时钟：Redis 的 TTL、otter 的到期、BigCache 的 `LifeWindow`。`ottercachex` 不经 `Encoded`，但同样按真实时钟算剩余时间。

所以 `WithNow` 的时钟离真实时间很远时，`Cache` 判断的三态和后端的过期会对不上。比如测试里从 1970 年开始的 `cachextest.Clock`：算出来的 `ExpiresAt` 在真实时钟看来早就过了，字节后端不写、otter 当场丢掉，每次读取都未命中。这类测试用 `cachextest.Map`，或者让时钟从 `time.Now()` 开始。

### 值是共享的

内存后端（`ottercachex`、`cachextest`）存的是值本身：`T` 是指针时，所有调用方拿到的是**同一个**对象，修改返回的值就是在修改缓存。存不会被修改的值，或者存副本。`bigcachex` 每次读都解码出新的值，没有这个问题。

## ottercachex

- 必须给出容量：`MaximumSize`（条目数），或 `MaximumWeight` 加 `Weigher`（比如按字节），两者正好选一个，否则 `New` 返回错误。内存层必须有上限。
- 每个条目在 `ExpiresAt` 过期（otter 的 `ExpiryCalculator`，按写入时的剩余时间计算，用的是真实时钟）。写入时已经过了 `ExpiresAt` 的条目立即过期：otter 把不大于零的剩余时间当作永不过期，所以至少给 1 纳秒。
- 写入同步可见，不需要等待缓冲区。otter 的淘汰在它自己的维护过程中进行，条目数可能短暂超过上限。
- 为什么选 otter，见 [ADR 0014](../adr/0014-otter-for-the-memory-layer.md)。

## bigcachex

- `New` 返回 `cachex.Backend[T]`。配置里传入一个已经建好的 `*bigcache.BigCache`。BigCache 所有条目共用一个寿命（`LifeWindow`），要设成不小于这一层最长的「新鲜期 + 陈旧期」，否则条目会提前消失。
- 字节后端的 `Set` 不看 `expiresAt`：腐烂的条目留在 BigCache 里，直到它自己的淘汰清掉，但不会被返回。
- 值存成字节，GC 不用扫描它们；代价是每次读都要解码。
- 没有原生批量操作，`GetMany`/`SetMany`/`DelMany` 逐个 key 进行，尽力而为。

## rediscachex

- `New` 返回 `cachex.Backend[T]`。每个条目一个字符串 key（`KeyPrefix + key`），用带 TTL 的 `SET` 写入，TTL 是 `ExpiresAt` 减去现在，至少 1 毫秒（go-redis 把负的 TTL 当作「保留原有 TTL」）；已经腐烂的条目由 `Encoded` 改为 `DEL`。
- 单个 key 的 `Set`/`Del` 失败返回普通的 error，不是 `*BatchError`。
- 读取用 `StringCmd.Bytes()` 拿回复，它和回复共用内存，不再复制一次。`T = []byte` 时，返回的切片直接共用这块内存；这个命令对象用完就丢弃，没有别人持有它，所以是安全的。

### 批量：pipeline

- `GetMany`/`SetMany`/`DelMany` 用 `GET`/`SET`/`DEL` 的 pipeline，而不是 `MGET`。这样在 Redis Cluster 上，一批 key 即使分布在不同的 slot 也能用。
- 大批量按 `ChunkSize`（默认 1000）切成多个 pipeline，**依次**发送。Redis 服务端是单线程的，并发发送多个 pipeline 收益很小。
- **每条命令的错误各自判断**：pipeline 的 `Exec` 只返回第一个失败命令的错误。Redis 回复的错误（例如对一个 list 执行 `GET` 返回 `WRONGTYPE`）已经记在各自的命令上；但**连接层面的失败**（连不上、连接中断）发生时，没发出去的命令自身是没有错误的。所以 `Exec` 返回的错误如果不是 Redis 的回复错误，就把它设到每一条还没有错误的命令上（`execPipe`）。否则 Redis 故障时，所有 `GET` 都会被当成未命中。
- 写入尽力而为：失败的 key（`Encoded` 编码失败、命令失败）列在 `*BatchError` 里，其余照常写入。

### 租约（`Leases`）

`Config.Leases` 打开租约（默认关闭），机制见 [consistency.md](consistency.md#租约lease)。

- **租约 key 和数据 key 在同一个 slot**：数据 key 带 hash tag 时，租约 key 是 `cachex-lease:` 加数据 key（hash tag 仍是第一个）；不带时是 `cachex-lease:{数据 key}`。含 `}` 却没有 hash tag 的 key 推不出来，`Cache` 拒绝它（[ADR 0019](../adr/0019-core-rejects-keys-without-a-redis-slot-for-their-lease.md)）。`cachex-lease:` 开头的 key 留给租约：`New` 拒绝这样开头的 `KeyPrefix`；同一个 Redis 里别的程序、或者不带前缀的 key，也不要用它开头，否则数据 key 会和某个租约 key 撞成同一个 Redis key。
- **领**：`SET 租约key 令牌 NX PX 有效期`，按 `ChunkSize` 走 pipeline。令牌是 128 位随机数。
- **回填、释放**：Lua 脚本，「租约仍是这个令牌」的核对和写入（或删除、释放）在一个原子操作里。pipeline 里用 `EVALSHA`；节点上没有脚本（重启、`SCRIPT FLUSH`）报 `NOSCRIPT` 的 key，再用 `EVAL` 发一次，顺带把脚本装上。
- **写入**：同一个 pipeline 里**先删租约 key、再写数据**。顺序反过来的话，写完数据、还没删租约的那一刻，持有旧令牌的回填仍能把旧值写进去。有测试截获 pipeline 检查这个顺序。
- 在真实的 Redis 7 和单节点 Cluster（`CROSSSLOT` 检查生效）上跑过 `TestLeaser`；平时的测试用 miniredis，它的 TTL 由测试按真实时间推进。

## gormcachex

### 表结构

```go
type row struct {
    Key       string    `gorm:"not null;primaryKey;size:255"`
    Value     []byte    `gorm:"not null"`       // Encoded 编码后的条目
    ExpiresAt time.Time `gorm:"not null;index"` // 给清理腐烂的行用
    UpdatedAt time.Time `gorm:"not null"`
}
```

`KeyPrefix` 会拼在 key 前面，同一张表可以给多个用途使用；key 列最长 255 个字符，**包括前缀**，而且必须是合法的 UTF-8、不含 NUL。别的 key 不进 SQL：读成未命中，写入只让它自己失败（回填失败只记 WARN，这个 key 就每次都未命中），删除什么也不做。数据库拒收一个 key 时会让整条语句失败，所以要先拦下，否则同一条语句里的其他 key 跟着失败（PostgreSQL 连读取都会失败）。`New` 返回 `*gormcachex.Backend[T]`，它嵌入 `cachex.Backend[T]`，另有 `Migrate` 建表。表不会自动清理，过了 `expires_at` 的行要自己定期删除（清理任务在 [todo](../todo.md) 里）。

### key 必须精确比较

缓存 key 区分大小写，也区分末尾空格。不同数据库的默认行为不同：

| 数据库 | key 列默认怎么比较 | gormcachex 的处理 |
|---|---|---|
| PostgreSQL、SQLite | 逐字节 | 不需要额外处理 |
| MySQL | 默认排序规则（`utf8mb4_0900_ai_ci`）**不区分大小写和重音** | `Migrate` 建表时使用 `utf8mb4_0900_bin`。建表前检查版本：低于 8.0.17 的 MySQL 和 MariaDB 都没有这个排序规则，会返回写明版本号的错误。表已经存在时，检查 `key` 列的排序规则，不是 `utf8mb4_0900_bin` 就返回错误，错误里带转换语句 |

`Migrate` 不替你改表。用 MySQL 默认配置建的表要自己转换：`ALTER TABLE <表> CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`。不调 `Migrate`、自己建表时，同样要用这个排序规则。

读写本身还多一道防线，万一碰上不能精确比较的表（比如没调 `Migrate`、自己建的表），也**不会读到别的 key 的值**：`Get`/`GetMany` 只返回存储的 key 和请求的 key **完全相等**的那一行；upsert 时连同 key 列一起改写，所以共用的一行只属于最后写入它的 key。代价是只差大小写的 key 会互相挤占同一行，表现为多几次未命中。决策过程见 [ADR 0009](../adr/0009-gorm-exact-keys-and-collation.md)。

### `key` 是 MySQL 的保留字

key 列一律以 `clause.Column` 传给 GORM（条件用 `clause.IN`，PostgreSQL 删除时的子查询写成 `Where("? IN (?)", keyColumn, …)`），由 GORM 给列名加引号。不能把列名直接写进 SQL 片段，比如 `Where("key = ?")`：SQLite 能接受，MySQL 会报语法错误。有一个单元测试会截获实际执行的 SQL，检查列名有没有加引号。

### 批量与死锁

- `GetMany`/`SetMany`/`DelMany` 按 `ChunkSize`（默认 1000，最大 8000：一行绑定 4 个参数，SQLite 一条语句最多 32766 个）切成多条语句，依次执行，**每条语句独立提交**，从不进调用方的事务（见下文）。某一段失败时，这一段的 key 记进 `*BatchError`，然后继续执行后面的段。单个 key 的方法就是只有一个 key 的批量。
- 已经腐烂的条目由 `Encoded` 改为删除，不写入。
- **统一加锁顺序**：
  - `SetMany` 插入前按 key 的字节序排序。否则多个实例同时回填有重叠的 key 时，会以相反的顺序锁行而死锁。
  - `DelMany` 同样先按 key 排序；PostgreSQL 上再用 `SELECT … ORDER BY key COLLATE "C" FOR UPDATE` 按字节序锁好行，再删除。`DELETE` 是按扫描顺序加锁的，而扫描顺序跟着列的排序规则走（官方镜像默认是 `en_US.utf8`），和 `SetMany` 的字节序不一致。
  - MySQL 的 `utf8mb4_0900_bin` 按码点排序，和 UTF-8 的字节序一致，所以不需要额外处理。
- **死锁重试**：MySQL 的间隙锁仍可能偶发死锁，InnoDB 要求应用自行重试。所以每条写语句被数据库判为死锁牺牲者时会重试，最多执行 5 次（即重试 4 次），每次退避的时间带随机抖动，并尊重 ctx。识别方式：PostgreSQL 看 SQLSTATE `40P01`/`40001`；MySQL 匹配驱动的错误文本 `Error 1213 (40001)`，因为 go-sql-driver 的错误类型没有提供 SQLState 方法。

### 隔离级别

只支持、也只测过各数据库的**默认**隔离级别：MySQL InnoDB 的 REPEATABLE READ、PostgreSQL 的 READ COMMITTED、SQLite 的 SERIALIZABLE。gormcachex 不设置隔离级别，每一段写入都是一条自动提交的语句，用的是连接的默认值；不要把缓存表所用连接的默认隔离级别改掉。

- MySQL 上剩下的偶发死锁正是 REPEATABLE READ 的间隙锁造成的，靠重试兜住。改用 READ COMMITTED 实测死锁反而更多。
- 统一加锁顺序之后，PostgreSQL 在默认级别下实测没有死锁，重试只是保险。

这几种做法的实测对比（包括锁表）见 [research/2026-10-gorm-deadlock.md](../research/2026-10-gorm-deadlock.md)，决策见 [ADR 0010](../adr/0010-gorm-deadlock-ordering-and-retry.md)。

### 事务

gormcachex 不进调用方的事务，每条语句都在 `Config.DB` 上独立提交，行锁不跨语句持有。写入要是留在事务里，行锁会跨 `Cache` 调用持有，和分片锁形成数据库检测不到的死锁，见 [ADR 0017](../adr/0017-gorm-never-joins-a-caller-transaction.md)。正确的用法是在事务里改数据源，提交之后再 `Set`/`Del`。

### 租约（`Leases`）

`Config.Leases` 打开租约（默认关闭），机制见 [consistency.md](consistency.md#租约lease)。`Migrate` 总会建两列 `lease_token`、`lease_until`（可为空）；不开租约时读写都不碰它们，所以没调 `Migrate` 的旧表照样能用，开租约才需要这两列。

- **领**：每段（至多 4000 个 key，SQLite 的参数上限）三条语句，各自提交，不管段多大：先给不存在的 key 插入只有租约、value 为空的行（冲突就跳过）；再用 `CASE` 把没人持有、或已经过期的租约改成这次的令牌；最后查一遍，令牌是自己的才算领到。三步之间被别人插进来，最后一步会如实反映。
- **回填**：一条 `UPDATE`，`value`、`expires_at` 用 `CASE` 按 key 取值，条件是每个 key 的 `key = ? AND lease_token = ?`，同时把租约清空；要删除的 key 用一条同样条件的 `DELETE`。被写入删掉的行不存在，条件更新碰不到它，旧值不会让它复活。
- **写入**：upsert 顺带把两列置空，和写值在同一条语句里；删除连行一起删掉。
- **释放**：先删只有租约的行（`LENGTH(value) = 0`），再清空其余行的租约。
- **读取**：跳过 value 为空的行（只有租约），不交给 `Encoded` 去解码，否则会被当成坏条目删掉。
- **有效期按各实例自己的时钟**：`lease_until` 由领租约的实例写入，别的实例按自己的时钟判断它过没过期，时钟偏差只影响租约提前或推迟过期。
- PostgreSQL 上的 `UPDATE`、`DELETE` 和删除一样，先按字节序锁行（`inLockOrder`）。`CASE` 的值在 PostgreSQL 上要写成 `?::bytea`、`?::timestamptz`，否则会被当成 text。
- **两个 GORM 的坑**：只有一个元素的 `clause.Or` 放进 `clause.And` 时，会渲染成 `… OR (…)`，等于把条件变成了「或」，所以单个 key 时直接用那个条件；`clause.Expr` 里紧跟 `(` 的 `?` 会把切片参数（包括 `[]byte`）展开成列表，所以类型转换不能写成 `CAST(? AS bytea)`。

## cachextest

- `Map[T]`：基于 `sync.Map`、没有容量上限、从不清理的 `Backend`，只用于测试。
- `Clock`：手动时钟，`Now` 传给 `cachex.WithNow`，`Advance` 拨动它。
- `TestBackend[T](t, newBackend, values)`：`Backend` 的契约测试。`values` 至少 3 个不同的值；用 `string` 和一个结构体各跑一遍，编码值的后端就覆盖了编解码器的主要分支。检查未命中不是错误、条目原样读回（包括不存在记录）、值和不存在记录互相覆盖、写入一个已经腐烂的条目不会让旧值还能读到、key 精确比较（大小写、末尾空格、重音）、批量操作、批量里重复的 key、空批量，以及多个 goroutine 同时读写（各自的 key 读回正确、共享 key 最后是写过的某个值，配合 `-race`）。
- `TestByteBackend(t, newBackend)`：`ByteBackend` 的契约测试：未命中不是错误、字节原样读回并能覆盖、传进去和拿出来的字节归调用方、key 精确比较、批量操作、空批量、多个 goroutine 同时读写。
- `NewLeasingMap[T]()`：打开了租约的 `Map`，几个 `Cache` 共用它，就是共用一个共享层的几个实例。
- `TestLeaser(t, newBackend, value)`：打开了租约的 `Backend` 的契约测试：租约只能领一次，直到结束或过期；`Set`、`SetMany`、`Del`、`DelMany` 都结束租约；`PutLeased` 只在令牌对得上时写入或删除，并结束租约；别的令牌既不能写也不能释放；`Release` 结束租约、不写；已经腐烂的条目不会让旧值还能读到；批量。

自写后端可以直接拿它们来测。
