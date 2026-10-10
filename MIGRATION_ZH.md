# 从 v1 迁移到 v2

[English](MIGRATION.md) | 中文

v2 是新的大版本：导入路径、API 和存储格式都变了。缓存数据可以丢弃，所以升级的代价是一次冷启动，不需要迁移数据。标了 † 的条目在 v1 最后几个版本（带 `GetMany` 的那些）里已经改过；如果你用的是那几个版本，这些不算新变化。

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
| `Cache[T]`（后端接口）、`BatchCache[T]` | `Backend[T]`，包含批量方法；只有单 key 能力的存储用 `SingleBackend[T]` 加 `Batched` |
| `Upstream[T]`、`UpstreamFunc[T]`、`BatchUpstream[T]` | `Source[T]`、`SourceFunc[T]`、`BatchSource[T]` |
| `ErrKeyNotFound`、`IsErrKeyNotFound(err)` | `ErrNotFound`、`errors.Is(err, cachex.ErrNotFound)`；`Cached`/`CacheState` 字段没有了 |
| `Entry[T]`、`EntryWithTTL`、`WithStale`、`WithServeStale` | 层选项 `TTL(fresh, stale)`：陈旧期大于零就是返回陈旧值。值就是普通的 `T` |
| `NotFoundWithTTL`、`WithNotFound`（第二个后端） | 层选项 `NotFoundTTL(fresh, stale)`，存在同一个后端里 |
| 按值内容自定义新鲜度判断 | 本身会过期的值用 `WithMaxAge(func(T) time.Duration)`；「整体作废」把版本号放进 key |
| `WithFetchConcurrency` | `WithFetchesPerKey` |
| `WithGetManyFetchConcurrency` | `WithGetManyConcurrency` |
| `WithGetManyChunkSize`、`WithFetchTimeout`、`WithLogger`、`WithDoubleCheck` | 名字不变，不再带类型参数 |
| `NowFunc`、`MockClock` | `WithNow(func() time.Time)`、`cachextest.Clock` |
| `DefaultFetchTimeout` 等 `Default*` 变量 | 常量 |
| `Transform`、`JSONTransform`、`StringJSONTransform` | 去掉了：存字节的后端接收 `Codec[T]`（默认 `DefaultCodec`） |
| `RistrettoCache` | `ottercachex.Backend`（必须设 `MaximumSize` 或 `MaximumWeight`） |
| `BigCache` | `bigcachex.Backend` |
| `RedisCache`、`RedisCacheConfig.TTL` | `rediscachex.Backend`；没有 TTL 配置，见下文 |
| `GORMCache` | `gormcachex.Backend`、`Migrate` |
| `WithGORMTx`、`GetGORMTx` | `gormcachex.WithTx`、`gormcachex.TxFrom` |
| `SyncMap` | `cachextest.Map`，只用于测试（没有容量上限） |
| — | `Cache.SetMany`、`Cache.DelMany`、`Cache.Close`、`IsShared` |

## 行为

**各层放在一个 `Cache` 里。** 一个 key 在所有层之间只有一次回源：最上层未命中时认领，依次读下面各层，再问数据源，然后回填到找到它的那层之上的每一层。往上复制的条目保留数据源回答的时间，所以年龄不会每层重新计时，也不会比下层的条目活得更久。*怎么做：* 用一个 `Cache` 配上所有层，不再串联多个 client。

**后端不再有自己的 TTL。** 每个条目都带着它什么时候变陈旧、什么时候腐烂，由所在层的 TTL 算出；Redis、otter 和数据表都把它当作条目的原生过期时间。*怎么做：* 去掉后端的 TTL 配置，在每层上设 `TTL`。

**抖动默认开启。** 每个条目的新鲜期随机缩短最多 10%（`DefaultJitter`），不会延长。*怎么做：* 不用处理；要关掉就用 `Jitter(0)`。

**写入从下往上，并且从不写数据源。** † `Set`、`Del`、`SetMany`、`DelMany` 先写最下层。某一层失败时，从这一层和它上面的每一层删掉这个 key，并返回错误。*怎么做：* 先改数据源，再通过同一个 `Cache` 写入。

**`Del` 只做失效。** v1 配置了不存在缓存时，`Del` 会记下「不存在」，于是更新之后的 `Del` 会让更新过的行在这条记录过期前都读不到。v2 只从每一层删掉这个 key，下次读取去问数据源。*怎么做：* 不用处理；如果你依赖 `Del` 表示「已删除」，下次读取会把它记下来。

**回源比发起它的调用方活得更久。** † 一次回源被这个 key 的所有请求共用，所以不会随发起它的调用方结束；它的 ctx 保留调用方的值，但不带它的取消，`IsShared(ctx)` 能识别它。`gormcachex` 在这样的 ctx 里不使用调用方的事务。*怎么做：* 不用处理；不要指望回填加入你的事务。

**回源 panic 或调用 `runtime.Goexit` 时，每个等待方都会收到错误。** † v1 里 Goexit 的回源的等待方要一直等到自己的 ctx 结束。

**`DoubleCheckAuto` 只在请求读过第一层之后、本 `Cache` 写过这个 key 所在分片时才再读一次。** † v1 里只要配置了不存在缓存就开启。要看到其他进程写进共享第一层的值，用 `DoubleCheckEnabled`。

**解不出来的数据算未命中。** v1 里编解码器读不了的值（比如类型改了之后），会让每次 `Get` 都报错，直到它过期。v2 的后端会删掉它、记一条警告，读取照常回源。

**存储格式变了。** Redis 或数据表里的 v1 数据 v2 读不了，会被当作未命中。*怎么做：* 换一个 `KeyPrefix`（或 Redis 库）；`gormcachex` 用一张新表（列变了：`value` 存编码后的条目，新增 `expires_at`），并运行 `Migrate`。

**MySQL 需要 8.0.17 及以上，不支持 MariaDB。** † `Migrate` 建表时 key 列用 `utf8mb4_0900_bin`；已有的表 key 列是别的排序规则时会报错，并给出转换用的 `ALTER TABLE` 语句。只支持各数据库默认的隔离级别。

**批量写是尽力而为。** † `Cache` 和每个后端的 `SetMany`/`DelMany` 都会尝试每个 key，把失败的列进 `*BatchError`。

**`GetMany` 一开始就认领全部 key。** 数据源不是 `BatchSource` 时，并发的 `Get` 碰上一次大 `GetMany` 里排着队的 key，要等它排到。*怎么做：* 用在 `GetMany` 里的数据源实现 `BatchSource`。

**`Close` 会等回源和后台刷新结束。** *怎么做：* 先调用它，再关闭后端。

**基准测试的数字变了。** v1 的 `BENCHMARK.md` 测的是一个被 sleep 主导的模拟；见新的 [BENCHMARK_ZH.md](BENCHMARK_ZH.md)。
