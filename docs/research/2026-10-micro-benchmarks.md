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

## v2

v2 重写了读写路径（见 [ADR 0012](../adr/0012-one-cache-over-layers.md)），这组基准随之移植到新接口：后端换成 `cachextest.Map`、ristretto 换成 otter，并新增 `Get/hit/bigcache`、`ZipfMixed/otter`。下面先比较 v1 和 v2，再记下移植时修掉的问题。

### 方法

- 机器同上（Apple M3 Pro）。Linux 的数字来自同一台 Mac 上 Docker Desktop 的 Linux 虚拟机（linux/arm64，12 个 CPU，Go 1.26.9），不是服务器。macOS 上用 Go 1.27.1。
- 测试时机器上还有别的负载（load average 10 到 20）。为了让负载对两版的影响相同，v1（提交 8683de5，即去掉 pkg/errors 之后）和 v2 各编译成一个测试二进制，交替运行 10 轮，每轮 `-benchtime 200ms`，再用 benchstat 比较。v1 的基准名按 v2 的叫法改名（`hit/syncmap` → `hit/map`，`hit/ristretto` → `hit/otter`，`ZipfMixed` → `ZipfMixed/map`）。
- 读时钟本身的成本单独测过：`time.Now()` 在 macOS 上约 30ns，在 Linux 虚拟机里约 38ns。

### v1 → v2

代码是提交 0a3f039。`Get/miss` 和 `Get/hit/l2` 每轮都调用 `cachex.Settle`，等回填完成（原因见下文「先发布、再回填」）。

| 基准 | Linux v1 | Linux v2 | macOS v1 | macOS v2 | 分配 v1 → v2 |
|---|---|---|---|---|---|
| `Get/hit/map/serial` | 16.9ns | 80.6ns | 16.9ns | 57.6ns | 0 → 0 |
| `Get/hit/map/parallel` | 2.2ns | 10.7ns | 2.4ns | 12.4ns | 0 → 0 |
| `Get/hit/otter/serial` | 37.6ns | 192ns | 36.8ns | 167ns | 0 → 0 |
| `Get/hit/otter/parallel` | 114ns | 24.3ns | 91.8ns | 20.6ns | 0 → 0 |
| `Get/notfound-hit` | 535ns | 81.4ns | 501ns | 56.9ns | 12 → 0 |
| `Get/stale-hit` | 104ns | 73.8ns | 87.9ns | 71.7ns | 2 → 1 |
| `Get/miss` | 2.74µs | 2.11µs | 2.74µs | 2.33µs | 26 → 22 |
| `Get/miss/x-sync-baseline` | 277ns | 134ns | 244ns | 127ns | 7 → 4 |
| `Get/hit/l2` | 2.49µs | 3.41µs | 2.55µs | 3.75µs | 23 → 24 |
| `GetMany/hit/n=100/GetMany` | 10.6µs | 11.8µs | 9.59µs | 10.5µs | 14 → 14 |
| `GetMany/hit/n=100/loop-Get` | 2.30µs | 8.55µs | 2.38µs | 6.15µs | 0 → 0 |
| `GetMany/half-miss/n=100/GetMany` | 66.2µs | 52.5µs | 55.5µs | 44.8µs | 598 → 约 200 |
| `GetMany/half-miss/n=100/loop-Get` | 154µs | 108µs | 157µs | 119µs | 1500 → 950 |
| `HotKeyStampede`（每轮） | 85.5µs | 70.7µs | 61.0µs | 51.2µs | 546 → 151 |
| `SetDel/spread` | 334ns | 168ns | 350ns | 202ns | 4 → 1 或 2 |
| `SetDel/same-stripe` | 374ns | 391ns | 270ns | 285ns | 6 → 4 |
| `ZipfMixed/map` | 202ns | 176ns | 153ns | 145ns | 4 → 1 |
| `ZipfMixed/otter`（只有 v2） | | 191ns | | 163ns | 1 |

- **命中变贵了，主要是读时钟**：v2 每次命中都按条目的时间判断新鲜度，而 v1 的命中基准存的是不带新鲜度判断的值。在 Mac 上做了公平比较：

  | | 每次命中 |
  |---|---|
  | v1，不带新鲜度（基准里的写法） | 19ns |
  | v1，`EntryWithTTL`（和 v2 一样要读时钟） | 43ns |
  | v2 | 59ns |

  剩下的 16ns 里，约 7ns 是读第一层之前先读分片纪元（`DoubleCheckAuto` 需要它，见 ADR 0008），去掉这一步实测过。把分片纪元挪到读取之后也试过：远端的第一层读得慢时，读取期间的回填会被漏掉，`TestDoubleCheck` 失败，所以没有采用。
- **不存在命中几乎不花钱**：只读一层、只读一次，`ErrNotFound` 原样返回。
- **otter 单 key 串行慢**：profile 显示约 80% 的时间在 `pthread_cond_signal/wait`，即 otter 的读缓冲攒满后唤醒维护 goroutine；cachex 自己约占 13%。并行、分散到很多 key 时，它比 v1 的 ristretto 快 4 到 5 倍。
- **`Get/hit/l2` 慢 37% 到 47%**：profile 里约 90% 是 goroutine 交接（线程唤醒和休眠），cachex 自身约 5%。

### 移植后做的性能修复

移植后第一次完整测量（提交 ba54dad 之前）有几项明显比 v1 慢，修复后（macOS，同一时段，`-count=5`）：

| 基准 | 修复前 | 修复后 | 做了什么 |
|---|---|---|---|
| `Get/hit/map/parallel` | 97ns | 12ns | 测试后端 `cachextest.Map` 从 `RWMutex` 换成 `sync.Map`：多核读时 RLock 的计数器在 CPU 间争用。v1 的 SyncMap 本来就是 `sync.Map` |
| `Get/miss` | 4.83µs，2432B，28 次分配 | 2.24µs，960B，21 次分配 | 只回填一个 key 时不再建 map；回填沿用所在调用的超时，不再另开一个 `WithTimeout` |
| `Get/hit/l2` | 5.0µs，3656B，33 次分配 | 3.9µs，1240B，24 次分配 | 下层只读一个 key 时用 `Get`，并且不建只有一个元素的 map（一个条目约 96 字节，一个 map 至少分配 8 个槽，近 1KB） |
| `SetDel/spread` | 493ns，849B，8 次分配 | 239ns，218B，4 次分配 | 单个 key 的 `Set`/`Del` 走单独的快路径，不建批量写用的 map 和切片 |
| `SetDel/same-stripe` | 1185ns | 约 430ns | 同上 |

### 先发布、再回填（c847c12、77cfd47）

回源现在先把结果交给等待方，再回填各层；回填完成之前，这次在途回源一直保持登记，所以紧接着的读取会加入它，而不是再回源一次。调用方因此不再等回填，上面的层在远端时，这能省下一次 Redis 或数据库的写入延迟。`Get/miss` 和 `Get/hit/l2` 改成每轮调用 `cachex.Settle` 等回填完成，测的总工作量和 v1 一样（v1 先回填再返回），所以这一项改动在这组内存基准里看不出收益：Linux 上 `Get/miss` 2.13µs → 2.11µs，`Get/hit/l2` 3.90µs → 3.41µs。

### 不用等待的写入不再分配（0a3f039）

分片空闲时，写入用 `TryLock` 直接拿到分片锁，不再为等锁准备 goroutine 和闭包。Mac 上：同一分片的 `Set`/`Del` 从约 435ns 降到约 296ns，分散的从约 239ns 降到约 200ns。交替测量的结果：同一分片和 v1 持平（Linux 374ns 对 391ns，macOS 270ns 对 285ns），分散的比 v1 快 42% 到 50%。

### 移植时发现的 bug

数据源实现了 `BatchSource`、而要找的 key 全在下层命中时，`askSource` 拿到空的 key 列表，`slices.Chunk(…, 0)` panic。panic 被接住了，而且结果已经发布，所以调用方照样拿到值，只是多一条 ERROR 日志。已有测试没覆盖「多层 + 批量数据源 + 下层命中」这个组合。修复时补了回归测试，并让 `Close` 等待它之前开始的回源结束（否则测试断言日志时 panic 还没发生）。

### 没解决的

- `BenchmarkHotKeyStampede` 偶尔断言失败（每轮回源 0.9995 次，而不是 1 次）：先发布、再回填之后，上一轮的回填可能落在下一轮开头的 `mem.Del` 之后，这一轮就读到了值、不回源。是基准的问题，不是 cachex 的：每轮结束时要调用 `cachex.Settle`。这一轮交替测量里 v2 有 2 到 4 个样本因此缺失，耗时数字仍然可用。
- otter 单 key 串行读的开销来自它的维护 goroutine，不在 cachex 里。
- 内存层全命中时，`GetMany` 每个 key 约 105 到 118ns，比循环 `Get` 慢；它的分配（去重、结果 map）是固定的。

## 复跑

```sh
go test -run '^$' -bench . -benchmem -benchtime 300ms -count=10 . > new.txt
go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt
```

机器上有别的负载时，像 v2 那样把两个版本各编译成测试二进制，交替运行：

```sh
go test -c -o v2.test .                       # 在 v2 的目录里
(cd ../v1 && go test -c -o ../v2/v1.test .)   # v1 的 worktree
for i in $(seq 10); do
  ./v1.test -test.run '^$' -test.bench . -test.benchmem -test.benchtime 200ms >> v1.txt
  ./v2.test -test.run '^$' -test.bench . -test.benchmem -test.benchtime 200ms >> v2.txt
done
go run golang.org/x/perf/cmd/benchstat@latest v1=v1.txt v2=v2.txt
```

Linux 上用 `GOOS=linux go test -c` 交叉编译，再在 `golang:1.26` 容器里运行同样的循环。

数字只在同一台机器上前后对比有意义。
