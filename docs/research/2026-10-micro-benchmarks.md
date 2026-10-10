# 微基准：cachex 自身的开销

日期：2026-10-10。代码：`benchmark_test.go`。

## 问题

原来的 `BenchmarkProductSearch` 每个请求都 `time.Sleep(1ms)`，是闭环负载：QPS 约等于并发数除以 sleep 时间，DB QPS 约等于 key 数除以新鲜期，测出来的主要是配置，不是库。它已经删掉。这一组微基准只测 cachex 自己：上游都是立即返回，命中路径上没有 sleep。

同时回答两个问题：

1. 去掉 `github.com/pkg/errors`、改用标准库，值不值得？
2. 审查时临时测到的两件事是否成立：缓存的「不存在」命中比普通命中慢很多；`GetMany` 全命中时每个 key 比循环 `Get` 慢。

## 方法

- Apple M3 Pro，darwin/arm64，GOMAXPROCS=12，后端除注明外都是 `SyncMap`。
- 每项跑 10 次（`-count=10 -benchtime 300ms`），用 benchstat 比较。表中是中位数。
- 各项测的是什么：

| 基准 | 测什么 |
|---|---|
| `Get/hit/{syncmap,ristretto}/{serial,parallel}` | 新鲜命中 |
| `Get/hit/l2` | L1 未命中、L2 命中，再回填 L1 |
| `Get/notfound-hit` | 命中新鲜的不存在记录 |
| `Get/stale-hit` | 命中陈旧值并触发（或跳过已在进行的）后台刷新 |
| `Get/miss` 和 `Get/miss/x-sync-baseline` | 一次完整回源（认领、回源、回填）；对照组是 `sync.Map` 加 x/sync 的 singleflight |
| `GetMany/{hit,half-miss}/n=…/{GetMany,loop-Get}` | 批量读对比循环 `Get`；half-miss 里一半 key 在上游不存在、没有不存在缓存，每次都回源 |
| `HotKeyStampede` | 64 个 goroutine 同时读一个刚被删掉的 key；断言每轮只回源 1 次 |
| `SetDel/{same-stripe,spread}` | 并行 `Set`/`Del`，同时有一个读者；key 全在一个分片里，或者分散开 |
| `ZipfMixed` | Zipf 分布的并行读，1 万个 key，其中 10% 在上游不存在 |

## 结果

### 去掉 pkg/errors 前后

| 基准 | pkg/errors | 标准库 | 变化 | 分配次数 |
|---|---|---|---|---|
| `Get/hit/syncmap/serial` | 17.3ns | 16.5ns | 无显著差别 | 0 → 0 |
| `Get/hit/ristretto/serial` | 37.3ns | 36.9ns | 无显著差别 | 0 → 0 |
| `Get/hit/l2` | 3.59µs | 2.53µs | -29% | 26 → 23 |
| `Get/notfound-hit` | 1.47µs | 0.48µs | **-67%** | 18 → 12 |
| `Get/stale-hit` | 87.6ns | 88.3ns | 无显著差别 | 2 → 2 |
| `Get/miss` | 3.78µs | 2.66µs | -30% | 29 → 26 |
| `Get/miss/x-sync-baseline` | 252ns | 246ns | 无显著差别 | 7 → 7 |
| `GetMany/half-miss/n=100/GetMany` | 76.2µs | 52.6µs | -31% | 748 → 598 |
| `GetMany/half-miss/n=100/loop-Get` | 241µs | 150µs | -38% | 1800 → 1500 |
| `HotKeyStampede`（每轮） | 78.8µs | 58.4µs | -26% | 743 → 546 |
| `ZipfMixed` | 219ns | 148ns | -33% | 5 → 4 |
| `SetDel/spread` | 336ns | 333ns | 无显著差别 | 4 → 4 |
| `SetDel/same-stripe` | 221ns | 255ns | +15% | 5 → 6 |
| 全部 25 项的几何平均 | | | **-19%** | |

- 命中路径不构造错误，所以没有变化。凡是经过「不存在」或回源的路径都快了 20% 到 67%：pkg/errors 每包一层都抓一次调用栈，而读路径在每次未命中时都会构造并包装 `ErrKeyNotFound`（后端未命中一次、上游不存在一次）。
- `SetDel/same-stripe` 慢了 15%，这条路径没有构造错误，分配次数的变化来自锁竞争下走了不同的分支。这一项对调度很敏感，判断为噪声，没有继续追。

### 两个发现

**缓存的「不存在」命中仍然比普通命中贵得多**：去掉 pkg/errors 后是 479ns、12 次分配，普通命中是 16.5ns、0 次分配，约 29 倍（之前约 85 倍）。剩下的开销来自：

- 先读后端（未命中，构造一个 `ErrKeyNotFound`），再读一次不存在缓存；
- 返回时用 `fmt.Errorf` 再包一层带 key 的错误。

结论成立，但量级比审查时的 70 倍小。合并条目与不存在记录（只读一次后端）、用不带格式化的错误，是进一步的方向。

**`GetMany` 全命中时，每个 key 比循环 `Get` 慢 4 到 7 倍**：

| n | `GetMany` | 循环 `Get` | 每 key（`GetMany` / 循环） |
|---|---|---|---|
| 10 | 1.14µs | 170ns | 114ns / 17ns |
| 100 | 9.49µs | 2.31µs | 95ns / 23ns |
| 1000 | 97.0µs | 25.4µs | 97ns / 25ns |

`GetMany` 全命中时固定有 14 到 20 次分配（去重、结果 map、按 key 的中间状态），而循环 `Get` 是 0 次。有未命中时情况反过来：half-miss 下 `GetMany` 比循环 `Get` 快 1.5 到 3.4 倍，因为只回源一次。结论成立：内存层全命中时，`GetMany` 的好处只在有未命中、或后端一次往返很贵（Redis、数据库）时才体现。

### 其他值得记下的数

- 一次完整回源 2.66µs，是「`sync.Map` + x/sync singleflight」对照组的 11 倍。多出来的是分片锁、写入代数检查、二次检查判断、不存在缓存的清理和错误包装。和任何真实上游的一次往返（通常是 100µs 以上）相比仍然很小。
- 热点 key 击穿：64 个并发读者，每轮上游只被调用 1 次（断言）。

## 复跑

```sh
go test -run '^$' -bench . -benchmem -benchtime 300ms -count=10 . > new.txt
go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt
```

数字只在同一台机器上前后对比有意义。
