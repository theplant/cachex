# 从 v1 迁移到 v2

[English](MIGRATION.md) | 中文

v2 是新的大版本：导入路径、API 和存储格式都变了。缓存数据可以丢弃，所以升级的代价是一次冷启动，不需要迁移数据。

## 导入路径

```go
import "github.com/theplant/cachex/v2"
```

各后端挪到了自己的包里：`github.com/theplant/cachex/v2/ottercachex`、`bigcachex`、`rediscachex`、`gormcachex`，测试用的在 `cachextest`。

## API

| v1 | v2 |
|---|---|
| `Client[T]`、`NewClient(backend, upstream, opts...)` | `Cache[T]`、`New(source, layers, opts...)` |
| 每层一个 `Client`，靠 `upstream` 串起来 | 一个 `Cache` 加 `[]Layer[T]`，最上层在前；每层是 `NewLayer(backend, layerOpts...)` |
| `Cache[T]`（后端接口） | `Backend[T]`，包含批量方法；只有单 key 能力的存储用 `SingleBackend[T]` 加 `Batched` |
| `Upstream[T]`、`UpstreamFunc[T]` | `Source[T]`、`SourceFunc[T]` |
| `ErrKeyNotFound`、`IsErrKeyNotFound(err)` | `ErrNotFound`、`errors.Is(err, cachex.ErrNotFound)`；`Cached`/`CacheState` 字段没有了 |
| `Entry[T]`、`EntryWithTTL`、`WithStale`、`WithServeStale` | 层选项 `TTL(fresh, stale)`：陈旧期大于零就是返回陈旧值。值就是普通的 `T` |
| `NotFoundWithTTL`、`WithNotFound`（第二个后端） | 层选项 `NotFoundTTL(fresh, stale)`，存在同一个后端里 |
| 按值内容自定义新鲜度判断 | 本身会过期的值用 `WithMaxAge(func(T) time.Duration)`；「整体作废」把版本号放进 key |
| `WithFetchConcurrency` | 没有对应：同一个 key 的并发未命中总是共用一次回源 |
| `WithFetchTimeout`、`WithLogger` | 名字不变，不再带类型参数 |
| `WithDoubleCheck` | 没有了：二次检查总是按需进行（见下文） |
| `NowFunc`、`MockClock` | `WithNow(func() time.Time)`、`cachextest.Clock` |
| `DefaultFetchTimeout` 等 `Default*` 变量 | 常量 |
| `Transform`、`JSONTransform`、`StringJSONTransform` | 去掉了：存字节的后端接收 `Codec[T]`（默认 `DefaultCodec`）；自己的字节存储实现 `ByteBackend`，再用 `Encoded(b, codec, logger)` 包一层 |
| `RistrettoCache` | `ottercachex.Backend`（必须设 `MaximumSize` 或 `MaximumWeight`） |
| `BigCache` | `bigcachex.New`，返回 `cachex.Backend[T]` |
| `RedisCache`、`RedisCacheConfig.TTL` | `rediscachex.New`，返回 `cachex.Backend[T]`；没有 TTL 配置，见下文 |
| `GORMCache` | `gormcachex.New`，返回带 `Migrate` 的 `*gormcachex.Backend[T]` |
| `WithGORMTx`、`GetGORMTx` | 已删除：`gormcachex` 从不加入你的事务，见下文 |
| `SyncMap` | `cachextest.Map`，只用于测试（没有容量上限） |
| — | `Cache.GetMany`，配合 `BatchSource[T]`、`WithGetManyConcurrency`、`WithGetManyChunkSize`；`Cache.SetMany`、`Cache.DelMany`、`Cache.Close`、`IsShared` |
| — | `cachextest.TestBackend(t, newBackend, values)` 和 `cachextest.TestByteBackend(t, newBackend)`，给自写后端用的契约测试 |

## 行为

**各层放在一个 `Cache` 里。** 一个 key 在所有层之间只有一次回源：最上层未命中时认领，依次读下面各层，再问数据源，然后回填到找到它的那层之上的每一层。往上复制的条目从复制时起按上层的 TTL 计时，但不会比下层那个条目更新鲜、更长寿，所以数据不会比最下层允许的更旧。*怎么做：* 用一个 `Cache` 配上所有层，不再串联多个 client。

**后端不再有自己的 TTL。** 每个条目都带着它什么时候变陈旧、什么时候腐烂，由所在层的 TTL 算出；Redis 和 otter 到时间就让条目原生过期；数据表里的行要你自己删除，BigCache 则留到它的 `LifeWindow` 结束。*怎么做：* 去掉后端的 TTL 配置，在每层上设 `TTL`。

**抖动默认开启。** 每个条目的新鲜期随机缩短最多 10%（`DefaultJitter`），不会延长。*怎么做：* 不用处理；要关掉就用 `Jitter(0)`。

**写入从下往上，并且从不写数据源。** `Set`、`Del`、`SetMany`、`DelMany` 先写最下层；v1 里每个客户端先写自己的后端，再写下一层。某一层失败时，从这一层和它上面的每一层删掉这个 key，并返回错误。*怎么做：* 先改数据源，再通过同一个 `Cache` 写入。

**`Del` 只做失效。** v1 配置了 `WithNotFound` 时，`Del` 会记下「不存在」，于是更新之后的 `Del` 会让更新过的行在这条记录过期前都读不到。v2 只从每一层删掉这个 key，下次读取去问数据源。*怎么做：* 不用处理；如果你依赖 `Del` 表示「已删除」，下次读取会把它记下来。

**含 `}` 却没有 hash tag 的 key 不再支持。** 比如 `a}b`：它在 Redis Cluster 上没法和自己的租约落在同一个 slot。不管用哪些层，`Get`、`GetMany`、`Set`、`SetMany` 对它返回 `ErrUnsupportedKey`，`Del`、`DelMany` 什么也不做。带 hash tag 的（`{user:1}:profile`）和只含 `{` 的 key 不受影响。*怎么做：* 换掉 key 里的 `}`，或者让它带上 hash tag。

**回源比发起它的调用方活得更久。** 一次回源被这个 key 的所有请求共用，所以不会随发起它的调用方结束；它的 ctx 保留调用方的值，但不带它的取消，`IsShared(ctx)` 能识别它。*怎么做：* 不用处理；你自己写的后端不能使用这样的 ctx 里属于调用方的状态，比如事务。

**回源 panic 或调用 `runtime.Goexit` 时，每个等待方都会收到错误。** v1 里 Goexit 的回源的等待方要一直等到自己的 ctx 结束。

**二次检查只在请求读过第一层之后、本 `Cache` 写过这个 key 所在分片时才再读一次，而且不再可配。** v1 里只要配置了 `WithNotFound` 就开启。总是再读几乎省不下什么，第一层被多个实例共享时也一样：它们在同一时刻未命中，谁也还没回填。*怎么做：* 去掉 `WithDoubleCheck`。

**解不出来的数据算未命中。** v1 里编解码器读不了的值（比如类型改了之后），会让每次 `Get` 都报错，直到它过期。v2 存字节的后端（经 `cachex.Encoded`）会删掉它、记一条警告，读取照常回源。

**存储格式变了。** Redis 或数据表里的 v1 数据 v2 读不了，会被当作未命中。*怎么做：* 换一个 `KeyPrefix`（或 Redis 库）；`gormcachex` 用一张新表（列变了：`value` 存编码后的条目，新增 `expires_at`），并运行 `Migrate`。

**`gormcachex` 从不加入你的事务。** v1 的 `WithGORMTx` 让缓存写入进你的事务；v2 没有这个选项，每条语句独立提交。写入留在事务里，行锁就会跨 `Cache` 调用持有，而 `Cache` 在进程内给同一个 key 的写入排队，两者可能永远互相等待。*怎么做：* 在事务里改数据源，提交之后再调用 `Set`/`Del`。

**后端按真实时钟让条目过期。** `WithNow`（v1 的 `NowFunc`）设的是 `Cache` 判断条目所用的时钟，但 Redis 和 otter 在条目的 `ExpiresAt` 按 `time.Now` 过去之后就丢掉它，存字节的后端也不写按 `time.Now` 已经过了 `ExpiresAt` 的条目。*怎么做：* 测试里用 `cachextest.Clock` 配这类后端时，让时钟从 `time.Now()` 开始。

**MySQL 需要 8.0.17 及以上，不支持 MariaDB。** `Migrate` 建表时 key 列用 `utf8mb4_0900_bin`；已有的表 key 列是别的排序规则时会报错，并给出转换用的 `ALTER TABLE` 语句。只支持各数据库默认的隔离级别。

**批量写是尽力而为。** `Cache` 和每个后端的 `SetMany`/`DelMany` 都会尝试每个 key，把失败的列进 `*BatchError`。

**`GetMany` 按段认领 key。** 默认 `GetMany` 未命中的 key 全部算一段，一开始就全部认领：数据源不是 `BatchSource` 时，并发的 `Get` 碰上一次大 `GetMany` 里排着队的 key，要等它排到。*怎么做：* 用在 `GetMany` 里的数据源实现 `BatchSource`，或者把 `WithGetManyChunkSize` 设成 `WithGetManyConcurrency` 左右：所在段还没开始的 key 没有被认领，`Get` 会当场自己回源。

**`Close` 会停止监听失效广播（如果开了），并等正在进行的回源和它们的回填、后台刷新，以及放弃等待的写入要补做的失效（和随后的广播发布）。** *怎么做：* 先调用它，再关闭后端。

**读取在回填之前返回。** 回源先把结果交给调用方，再写进上面的层；这期间来的读取直接拿到同一个结果，不会再回源。所以 `Get` 刚返回时，层里可能还没有这个值。*怎么做：* 一般不用管；测试里要在读完之后马上检查后端的，或者进程读完马上退出的（命令行工具），先调用 `Close`。

**基准测试的数字变了。** v1 的 `BENCHMARK.md` 测的是一个被 sleep 主导的模拟；见新的 [BENCHMARK_ZH.md](BENCHMARK_ZH.md)。
