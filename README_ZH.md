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

内存在 Redis 前面，两者都在数据库前面。每层有自己的 TTL；条目从 Redis 复制到内存时，保留数据源回答的时间，所以年龄不会重新计时：

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
tokens := cachex.New(source, []cachex.Layer[*oauth2.Token]{
    cachex.NewLayer(mem, cachex.TTL(time.Hour, 0)),
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

## 选项

| 选项 | 默认值 | 作用 |
|---|---|---|
| `TTL(fresh, stale)`（层） | 必填 | 条目新鲜多久，之后还能边刷新边返回多久 |
| `NotFoundTTL(fresh, stale)`（层） | `0, 0`：不记录 | 「不存在」的同样两个时长 |
| `Jitter(ratio)`（层） | `0.1` | 把每个条目的新鲜期最多缩短这个比例，让同时写入的条目不在同一时刻过期 |
| `WithMaxAge(func(T) time.Duration)` | 无 | 按值限制在每一层保存多久 |
| `WithFetchTimeout(d)` | 60s | 回源的每一次调用的上限：读下层、问数据源、回填 |
| `WithFetchesPerKey(n)` | 1 | 同一个 key 最多同时几次回源；1 表示并发未命中全部合并 |
| `WithGetManyConcurrency(n)` | 16 | 一次 `GetMany` 最多同时向数据源发几个请求 |
| `WithGetManyChunkSize(n)` | 0：一次调用 | 把一次 `BatchSource` 调用切成最多 n 个 key 的分段 |
| `WithDoubleCheck(mode)` | `DoubleCheckAuto` | 认领回源的请求什么时候先再读一次第一层 |
| `WithLogger(l)` | `slog.Default()` | 记录不让调用失败的错误（回填、后台刷新） |
| `WithNow(f)` | `time.Now` | 时钟；测试用 `cachextest.Clock` 手动推进 |

## 后端

| 包 | 存什么 | 说明 |
|---|---|---|
| `ottercachex` | 内存里的值 | 必须设 `MaximumSize`（条目数）或 `MaximumWeight` 加 `Weigher`；每个条目各自到期 |
| `bigcachex` | 内存里编码后的条目 | 数百万条目、GC 在 profile 里显眼时使用；每次读取都要解码 |
| `rediscachex` | Redis 或 Redis Cluster 里编码后的条目 | 每个条目原生过期；批量调用是每段 `ChunkSize` 个 key 的 pipeline |
| `gormcachex` | 数据表里编码后的条目 | 先调 `Migrate`；MySQL 需要 8.0.17 及以上，key 列为 `utf8mb4_0900_bin`；只在各数据库默认隔离级别下测过 |
| `cachextest` | 内存里的值，没有上限 | 只用于测试，另有 `Clock` 和给自写后端用的契约测试 `TestBackend` |

自己的存储实现 `Backend[T]`，或者实现 `SingleBackend[T]` 再用 `cachex.Batched` 包一层。存字节的存储用 `cachex.EncodeEntry`/`DecodeEntry` 和一个 `Codec` 编码条目。

## 必须知道

- **「不存在」就是 `ErrNotFound`。** 用 `errors.Is` 判断。缓存的 nil 是值，不是未命中。
- **`GetMany` 每个 key 有三种结果：** 在 map 里（找到），既不在 map 里也不在错误里（不存在），列在 `*BatchError` 里（失败）。
- **先改数据源，再通过同一个 `Cache` 写入。** 顺序保证只在一个进程里的同一个 `Cache` 内成立。
- **跨实例只靠 TTL 兜底。** 其他实例的内存层会一直保留旧值，直到它的条目腐烂；每层的 TTL 按「能接受旧数据多久」来设。
- **内存后端返回的就是存进去的那个值。** 不要修改从 `ottercachex` 或 `cachextest` 拿到的值。
- **值的类型有不兼容的改动时，换 `KeyPrefix`。** JSON 解码很宽松：改了名的字段会读成零值，而不是报错。
- **先 `Close`，再关闭后端。** 它会等正在进行的回源和刷新结束。

## 文档

- [常见问题](docs/faq_ZH.md)（[English](docs/faq.md)）
- [从 v1 迁移](MIGRATION_ZH.md)
- [基准测试](BENCHMARK_ZH.md)
- 设计文档：[设计总览](docs/design.md)、[术语表](GLOSSARY.md)、[决策记录（ADR）](docs/adr/)

## 许可证

[MIT](LICENSE)
