# 基准测试

[English](BENCHMARK.md) | 中文

这些是测 cachex 自身开销的微基准。数据源都立即返回，没有任何 sleep，所以数字是库每次调用的成本，而不是模拟出来的 I/O。一次真实的回源（一次 Redis 往返、一次数据库查询）要 100µs 以上，根据这里的数字做决定之前，先和这个量级比一比。

代码在 [`benchmark_test.go`](benchmark_test.go)；完整报告，包括 v1 的基线和移植这组基准时做的修复，见 [docs/research/2026-10-micro-benchmarks.md](docs/research/2026-10-micro-benchmarks.md)。

## 怎么跑

```sh
go test -run '^$' -bench . -benchmem -count=10 . > new.txt
go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt
```

数字只有在同一台机器上前后对比才有意义。

## 环境和方法

| | macOS | Linux |
|---|---|---|
| 机器 | Apple M3 Pro，12 核 | 同一台 Mac 上 Docker Desktop 的 Linux 虚拟机，12 个 CPU，7.75 GiB |
| 系统 | macOS 27.0.1，darwin/arm64 | linux/arm64 |
| Go | 1.27.1 | 1.26.9 |

Linux 的数字来自笔记本上的虚拟机，不是服务器。测试时机器上还有别的负载（load average 在 10 左右），所以 v1 和 v2 的测试二进制交替运行，各跑 10 轮 `-benchtime 200ms`，再用 benchstat 比较（下表是中位数）。并行的基准波动很大（最多 ±50%），只有明显超出这个范围的差别才有意义。

## 结果：v1 → v2

每次操作的耗时，取中位数。v1 是 v1 的最后一版代码（已去掉 `github.com/pkg/errors`），基准名换成了 v2 的叫法。

| 基准 | 测什么 | Linux v1 | Linux v2 | macOS v1 | macOS v2 | 分配次数 v1 → v2 |
|---|---|---|---|---|---|---|
| `Get/hit/map/serial` | 新鲜命中，内存层 | 17.8ns | 86.3ns | 17.7ns | 58.7ns | 0 → 0 |
| `Get/hit/map/parallel` | 同上，12 个 goroutine | 2.5ns | 11.3ns | 2.4ns | 11.4ns | 0 → 0 |
| `Get/hit/otter/parallel` | 新鲜命中，内存层（v1 是 ristretto，v2 是 otter） | 126ns | 28.2ns | 105ns | 23.6ns | 0 → 0 |
| `Get/notfound-hit` | 命中新鲜的不存在记录 | 552ns | 86.0ns | 500ns | 57.9ns | 12 → 0 |
| `Get/stale-hit` | 命中陈旧条目，并发起后台刷新 | 105ns | 74.0ns | 88.4ns | 70.7ns | 2 → 1 |
| `Get/miss` | 完整回源：认领、问数据源、回填 | 2.82µs | 2.13µs | 2.78µs | 2.06µs | 26 → 21 |
| `Get/miss/x-sync-baseline` | `sync.Map` 加 `x/sync/singleflight`，作为参照 | 274ns | 160ns | 263ns | 142ns | 7 → 4 |
| `Get/hit/l2` | 第一层未命中、第二层命中，再回填 | 2.61µs | 3.90µs | 2.61µs | 3.92µs | 23 → 24 |
| `GetMany/hit/n=100/GetMany` | 100 个 key，全部命中 | 11.6µs | 12.2µs | 10.1µs | 11.0µs | 14 → 14 |
| `GetMany/hit/n=100/loop-Get` | 同样的 key，循环调用 `Get` | 2.40µs | 8.80µs | 2.40µs | 6.40µs | 0 → 0 |
| `GetMany/half-miss/n=100/GetMany` | 100 个 key，一半在数据源里不存在，也没有缓存「不存在」 | 66.8µs | 55.2µs | 56.5µs | 43.2µs | 598 → 210 |
| `GetMany/half-miss/n=100/loop-Get` | 同样的 key，循环调用 `Get` | 155µs | 108µs | 159µs | 105µs | 1500 → 900 |
| `HotKeyStampede` | 64 个并发请求同时未命中一个 key；两版每轮都只问数据源 1 次 | 85.7µs | 66.3µs | 65.9µs | 50.3µs | 546 → 151 |
| `SetDel/spread` | 并行 `Set`/`Del` 1024 个 key，同时有一个读者 | 339ns | 267ns | 371ns | 244ns | 4 → 4 |
| `SetDel/same-stripe` | 同上，但所有 key 都在一个分片里 | 370ns | 528ns | 283ns | 351ns | 6 → 7 |
| `ZipfMixed/map` | 按 Zipf 分布并行读 1 万个 key，10% 在数据源里不存在 | 262ns | 224ns | 182ns | 149ns | 4 → 2 |
| `ZipfMixed/otter` | 同上，后端是 otter（只有 v2） | | 230ns | | 194ns | 2 |

## 这些数字说明什么

- **v2 的一次命中更贵，大头是读时钟。** 每次命中都要判断条目是否新鲜，这需要当前时间：单是 `time.Now()` 在 macOS 上约 30ns，在 Linux 虚拟机里约 38ns。v1 的命中基准存的是不带新鲜度判断的值；v1 里用 `Entry[T]` 的用户同样要付读时钟的开销。剩下的部分（找到 key 的分片、复制条目）约 25ns。
- **不存在命中几乎不花钱了**：不存在记录是层里条目的一种状态，只读一次，`ErrNotFound` 原样返回（不包装，不分配）。v1 要再读一次另一个后端，并格式化一个错误。
- **未命中和回源更便宜**（-25%，21 次分配，原来是 26 次）；64 个请求同时未命中，仍然只问一次数据源。
- **`GetMany` 只在有未命中、或者层在远端时才划算。** 内存层全部命中时，每个 key 约 110 到 120ns，而循环 `Get` 是 64 到 88ns；有未命中时，比 v1 快 17% 到 30%，并且只回源一次。面对 Redis 或数据库时，真正起作用的是一批 key 只往返一次，而不是每个 key 一次。
- **otter 的单 key 串行基准不具代表性**：一个 goroutine 反复读同一个 key，会不断唤醒 otter 的维护 goroutine，所以 `Get/hit/otter/serial` 约 165 到 200ns。分散到很多 key 和 goroutine 上时（`Get/hit/otter/parallel`、`ZipfMixed/otter`），它比 v1 的 ristretto 层快 4 到 5 倍。
- **同一分片里的写入比 v1 慢**（+24% 到 +43%）：这样的写入要排队等分片锁，而且每次写入都要为抖动取一个随机数。分散在不同分片上的写入快 20% 到 35%。
- `Get/hit/l2` 比 v1 慢 50%。它的时间主要花在 goroutine 交接上（回源在单独的 goroutine 里运行，这样调用方可以先走），花在 cachex 内部的 CPU 很少。
