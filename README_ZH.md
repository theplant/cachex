# cachex

> Go 的多层读穿透缓存：同一个 key 的并发未命中只回源一次，写入按同一顺序到达每一层，每一层各自管理新鲜度。

[![Go Reference](https://pkg.go.dev/badge/github.com/theplant/cachex/v2.svg)](https://pkg.go.dev/github.com/theplant/cachex/v2)
[![License](https://img.shields.io/github/license/theplant/cachex)](LICENSE)

[English](README.md) | 中文

`Cache` 从上到下读各层（比如先内存、再 Redis、再数据库表），只有所有层都没有可用条目时才问数据源。从 v1 升级？见 [MIGRATION_ZH.md](MIGRATION_ZH.md)。

## 安装

```bash
go get github.com/theplant/cachex/v2
```

## 快速上手

一个内存层，加一个数据源：

```go
mem, err := ottercachex.New[*Product](ottercachex.Config[*Product]{MaximumSize: 100_000})
if err != nil {
    log.Fatal(err)
}

source := cachex.SourceFunc[*Product](func(ctx context.Context, id string) (*Product, error) {
    p, err := loadProduct(ctx, id) // 你的数据库查询
    if errors.Is(err, gorm.ErrRecordNotFound) {
        return nil, cachex.ErrNotFound // 「不存在」用 ErrNotFound 表示
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
    // 没有这个商品
case err != nil:
    // 某一层或数据源出错
}
```

`TTL(fresh, stale)`：一分钟内条目直接返回；之后的十分钟里仍然立即返回，同时在后台刷新；再往后就不再返回。

## 两层

内存在 Redis 前面，两者都在数据库前面。每层有自己的 TTL；条目从 Redis 复制到内存后，按内存层的 TTL 保留，但不会比 Redis 里那个条目更新鲜、更长寿：

```go
mem, _ := ottercachex.New[*Product](ottercachex.Config[*Product]{MaximumSize: 100_000})
shared := rediscachex.New[*Product](rediscachex.Config[*Product]{Client: rdb, KeyPrefix: "product:v1:"})

products := cachex.New(source, []cachex.Layer[*Product]{
    cachex.NewLayer[*Product](mem,
        cachex.TTL(30*time.Second, time.Minute),
        cachex.NotFoundTTL(5*time.Second, 0)), // 「不存在」记 5 秒
    cachex.NewLayer[*Product](shared,
        cachex.TTL(5*time.Minute, time.Hour),
        cachex.NotFoundTTL(30*time.Second, 0)),
}, cachex.WithFetchTimeout(3*time.Second))
```

本身会过期的值（比如 token）用 `WithMaxAge` 按值设寿命上限；没法加方法的第三方类型也能用：

```go
tokenMem, _ := ottercachex.New[*oauth2.Token](ottercachex.Config[*oauth2.Token]{MaximumSize: 10_000})
tokens := cachex.New(tokenSource, []cachex.Layer[*oauth2.Token]{ // tokenSource 是一个 cachex.Source[*oauth2.Token]
    cachex.NewLayer[*oauth2.Token](tokenMem, cachex.TTL(time.Hour, 0)),
}, cachex.WithMaxAge(func(t *oauth2.Token) time.Duration {
    return time.Until(t.Expiry) - time.Minute // 快过期的 token 不再返回
}))
```

## 写入

先改数据源，再通过同一个 `Cache` 写入：

```go
if err := db.WithContext(ctx).Save(p).Error; err != nil { // 1. 数据源
    return err
}
return products.Set(ctx, p.ID, p) // 2. 每一层，从下往上
```

`Set` 从下往上写每一层；`Del` 从每一层删掉这个 key，下次读取去问数据源。两者都不写数据源。写入之前开始的读取，不会在写入之后把旧值放回去。`SetMany` 和 `DelMany` 对多个 key 做同样的事。

## 批量读

`GetMany` 每层一次调用读多个 key。数据源也实现了 `BatchSource` 时，未命中的 key 也一次回源：

```go
// GetMany 让 productSource 成为 cachex.BatchSource：没有返回的 key 就是不存在。
func (s productSource) GetMany(ctx context.Context, ids []string) (map[string]*Product, error) {
    var rows []*Product
    if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err != nil {
        return nil, err // 所有 key 都失败
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
    // be.Errors 列出失败的 key；found 里有其余所有 key
}
```

## 多个实例

几个进程共用一个 Redis 层时，可能出现这种情况：一个进程从数据源读到一个值，另一个进程随后改了数据源并调用 `Set`，第一个进程在这次写入之后才把它的旧值写进 Redis。于是每个实例都会把旧值复制进自己的内存层，直到 Redis 里的条目腐烂。打开租约就能避免：

```go
shared := rediscachex.New[*Product](rediscachex.Config[*Product]{Client: rdb, KeyPrefix: "product:v1:", Leases: true})
```

`gormcachex` 也有同样的 `Leases` 选项，给多个实例共用的表用（要先调 `Migrate`）。一次回源在 Redis 未命中的每个 key 上领租约，写入这个 key 会结束租约，回源只有还持有租约时才写 Redis。别的实例持有租约时，未命中照样返回结果，只是不写 Redis；后台刷新则把这个 key 交给那个实例。租约管不到其他实例的内存层。要让写入也到达那里，打开失效广播：一次写入之后，每个实例都从自己的内存层里删掉写过的 key。

```go
products := cachex.New(source, layers, cachex.WithBroadcast(
    rediscachex.NewBroadcast(rediscachex.BroadcastConfig{Client: rdb, KeyPrefix: "product:v1:"})))
```

它用 Redis Pub/Sub（Redis Cluster 也可以）；没有 Redis、但数据库是 PostgreSQL 时，`gobuscachex.NewBroadcast` 用 go-bus 做同样的事，每个实例一个队列，轮询取消息。实例断线期间发布的消息会丢，所以每次重新订阅时，它都清空自己的内存层：丢一条消息的代价是内存层变冷，而不是读到旧值。广播只碰后端在进程内的层（`ottercachex`、`bigcachex`，或者自己实现了 `cachex.LocalBackend` 的后端），开了广播就至少要有一个这样的层。发布失败只记日志，不返回错误。租约和失效广播都打开时，Redis 层的 TTL 可以设长，一次写入在消息到达时就在每个实例上可见。

## 选项

| 选项 | 默认值 | 作用 |
|---|---|---|
| `TTL(fresh, stale)`（层） | 必填 | 条目新鲜多久，之后还能边刷新边返回多久 |
| `NotFoundTTL(fresh, stale)`（层） | `0, 0`：不记录 | 「不存在」的同样两个时长 |
| `Jitter(ratio)`（层） | `0.1` | 把每个条目的新鲜期最多缩短这个比例，让同时写入的条目不在同一时刻变陈旧、腐烂 |
| `WithMaxAge(func(T) time.Duration)` | 无 | 按值限制在每一层保存多久；回源拿到的值和 `Set`/`SetMany` 写入的值都会经过它 |
| `WithFetchTimeout(d)` | 60s | 各自限时：回源的二次检查、读下面每一层连同它的回填、每次问数据源连同它的回填、写入失败后的失效 |
| `WithGetManyConcurrency(n)` | 16 | 一次 `GetMany` 最多同时向数据源发几个请求 |
| `WithGetManyChunkSize(n)` | 0：全部一段 | 把 `GetMany` 未命中的 key 切成最多 n 个一段回源，每段开始时才认领；所在段还没开始的 key，`Get` 不用等 |
| `WithLogger(l)` | `slog.Default()` | 记录不让调用失败的错误（回填、失效、后台刷新）和回源里的 panic |
| `WithNow(f)` | `time.Now` | 计算和判断条目所用的时钟（后端的原生过期仍按真实时钟）；测试用 `cachextest.Clock` 手动推进 |

## 后端

| 包 | 存什么 | 说明 |
|---|---|---|
| `ottercachex` | 内存里的值 | 必须设 `MaximumSize`（条目数）或 `MaximumWeight` 加 `Weigher`；每个条目各自到期 |
| `bigcachex` | 内存里编码后的条目 | 数百万条目、GC 在 profile 里显眼时使用；每次读取都要解码 |
| `rediscachex` | Redis 或 Redis Cluster 里编码后的条目 | 每个条目原生过期；批量调用是每段 `ChunkSize` 个 key 的 pipeline；多个实例共用时打开 `Leases`；`NewBroadcast` 提供失效广播 |
| `gormcachex` | 数据表里编码后的条目 | 先调 `Migrate`；MySQL 只支持 8.0.17 及以上；key、隔离级别、过期的行见注意事项；多个实例共用时打开 `Leases` |
| `goquecachex` | 不存条目：把 `gormcachex` 的清理做成 go-que 任务 | 队列需要 PostgreSQL（被清理的表可以在别处）；每个实例都调用 `StartCleanup`，同一时刻只有一个实例在跑 `DeleteExpired` |
| `gobuscachex` | 不存条目：用 go-bus 做失效广播 | 只支持 PostgreSQL；给有 PostgreSQL、没有 Redis 的部署用；每个实例订阅自己的队列，靠心跳续命 |
| `cachextest` | 内存里的值，没有上限 | 只用于测试，另有 `Clock`、`NewLeasingMap`（打开了租约的共享 map，几个代表不同实例的 `Cache` 共用它）、`NewBus`（内存里的失效广播，可以模拟丢消息），以及给自写后端和传输用的契约测试 `TestBackend`、`TestByteBackend`、`TestLeaser`、`TestBroadcast` |

`rediscachex.New` 和 `bigcachex.New` 返回 `cachex.Backend[T]`；`gormcachex.New` 返回 `*gormcachex.Backend[T]`，它也是一个 `cachex.Backend[T]`，另有 `Migrate`。

自己的存储实现 `Backend[T]`，或者实现 `SingleBackend[T]` 再用 `cachex.Batched` 包一层。存字节的存储实现 `cachex.ByteBackend`，再用 `cachex.Encoded` 包一层：它用 `Codec` 编码条目，把解不出来的数据当作未命中，已经腐烂的条目不写、改为删除。

## 注意事项

每一条：常见的错误用法，会发生什么，应该怎么做。

**数据源**

- **不存在的 key 返回 `nil, nil`。** 零值会被当成真实的值缓存起来。应该返回 `cachex.ErrNotFound`（包装一层也可以）。
- **`BatchSource.GetMany` 返回 `ErrNotFound`。** 这次调用的所有 key 都会读成不存在，但什么也不缓存，所以每次读取都会再回源。不存在的 key 不放进 map 就行；部分 key 失败时，原样返回 `*cachex.BatchError`，不要再包装。
- **在数据源里依赖调用方的取消。** 一次回源由等这个 key 的所有调用方共用，所以它的 ctx 不会因为某个调用方离开而结束，而是由 `WithFetchTimeout` 限时，并保留调用方的值。不要使用里面属于某个调用方的状态，比如数据库事务（`cachex.IsShared(ctx)` 能判断这是回源用的 ctx）。
- **在数据源里对同一个 key 调用同一个 `Cache`。** 这次调用会等它自己所在的那次回源，直到回源超时才失败。
- **数据源没有 `GetMany`，却有大批量的 `GetMany` 调用。** key 会逐个回源（同时最多 `WithGetManyConcurrency` 个），默认情况下并发的 `Get` 碰上还在排队的 key，要等它轮到。请实现 `BatchSource`，或者把 `WithGetManyChunkSize` 设成 `WithGetManyConcurrency` 左右。

**读取**

- **把 `GetMany` 返回的 `err != nil` 当成「什么都没找到」。** map 里有所有成功的 key，只有 `*BatchError` 里列出的 key 失败了。两边都没有的 key 是不存在。
- **用 `m[k] == nil` 判断 key 在不在。** 缓存的 nil 也是值。要用 `v, ok := m[k]`。
- **以为永远不会返回陈旧值。** 陈旧期大于零时，过了新鲜期的值会立即返回，同时在后台刷新。不能接受时把陈旧期设为零。
- **自带过期时间的值（token、签名 URL）按层的 TTL 缓存。** 用 `WithMaxAge`，让条目不会比值本身活得更久。
- **以为读不了的层会被跳过。** 某一层读失败时，这次读取直接失败，而不是把所有请求都打到数据源。
- **`Get` 刚返回就检查后端。** 结果是先返回、再写进各层的。测试里先调用 `Close`。
- **key 里有 `}` 却没有 hash tag**（比如 `a}b`）。这种 key 不支持：读写返回 `ErrUnsupportedKey`。带 hash tag（`{user:1}:profile`）或者不用 `}`。

**写入**

- **先写缓存再写数据源，或者只写缓存。** `Set` 和 `Del` 从不写数据源。先改数据源，再对同一个 `Cache` 调用 `Set` 或 `Del`；顺序反过来，中间的读取可能把旧值重新缓存进去。
- **以为一个进程的写入会到达另一个进程的内存层。** 不会；那里只靠 TTL 限制旧数据活多久。打开失效广播，或者把内存层的 TTL 设成能接受的程度；共用的 Redis 要打开租约（见「多个实例」），否则它的 TTL 也决定旧数据能活多久。
- **以为 `Del` 会记下「这个 key 已经不存在」。** 它只删除条目；下次读取会去问数据源。
- **忽略 `Set` 或 `Del` 的错误。** 失败那一层下面的层可能已经是新值；失败的层和它上面的层会删掉这个 key（尽力而为）。数据源保持你改过的样子。

**值和后端**

- **修改从内存后端拿到的值**（`ottercachex`、`cachextest.Map`）。它就是存进去的那个值，所有读取方共享。把值当作只读，或者先复制。
- **值的类型有不兼容的改动，却不换 `KeyPrefix`。** JSON 解码很宽松：改了名的字段会读成零值。改类型时一起换 `KeyPrefix`。
- **`ottercachex` 没设上限。** `New` 会失败：设置 `MaximumSize`，或者 `MaximumWeight` 加 `Weigher`。
- **`bigcachex` 的 `LifeWindow` 比层的 TTL 短。** 条目会提前消失。只在内存层有数百万条、profile 里 GC 明显时才用它；每次读取都要解码。
- **`gormcachex` 不调 `Migrate`，或者 MySQL 表不是 `utf8mb4_0900_bin`。** `Migrate` 会建表，并拒绝 key 列用其他任何排序规则的已有 MySQL 表。key 连同 `KeyPrefix` 最长 255 个字符，必须是合法的 UTF-8、不含 NUL；别的 key 总是未命中，写入失败。过期的行不会自动删除：定期调用 `DeleteExpired`（`goquecachex.StartCleanup` 让一个实例定时跑它，需要 PostgreSQL）。只支持各数据库默认的隔离级别。
- **在修改数据源的事务里调用 `Set`/`Del`。** `gormcachex` 从不加入事务：每条语句独立提交，事务回滚不会撤销缓存的写入。在提交之后再调用。
- **`cachextest.Clock` 远远落后于真实时钟，后端又会让条目过期。** 不管 `WithNow` 怎么设，后端都按真实时钟让条目过期，所以按这个时钟算出的条目会被当场丢掉，每次读取都未命中。让时钟从 `time.Now()` 开始，或者用 `cachextest.Map`。

- **没有 PostgreSQL 却用 `gobuscachex`。** go-bus 只支持 PostgreSQL：数据库是 MySQL 的话，为了广播还得另起一个 PostgreSQL。数据库是 PostgreSQL、又没有 Redis 时才用它；其他情况用 `rediscachex.NewBroadcast`。

**生命周期**

- **`Cache` 还在工作时就关闭后端。** 先调用 `Close`：它会等正在进行的回源和它们的回填、后台刷新，以及放弃等待的写入要补做的失效。
- **读完马上退出进程**（命令行工具、定时任务）。读取在结果写进各层之前就返回了，进程可能在 Redis 或数据表写好之前就结束，下次运行又得回源。退出前调用 `Close`。

## 文档

- [常见问题](docs/faq_ZH.md)（[English](docs/faq.md)）
- [从 v1 迁移](MIGRATION_ZH.md)
- [基准测试](BENCHMARK_ZH.md)
- 设计文档：[设计总览](docs/design.md)、[术语表](GLOSSARY.md)、[决策记录（ADR）](docs/adr/)

## 许可证

[MIT](LICENSE)
