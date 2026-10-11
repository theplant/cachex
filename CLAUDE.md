# cachex

Go 泛型多层读穿缓存（module `github.com/theplant/cachex/v2`）：一个 Cache 管从上到下的若干层和一个数据源，合并回源、二次检查、不存在记录、返回陈旧值、抖动、寿命上限、批量读写、写入顺序保证；后端在子包里：`ottercachex`、`bigcachex`、`rediscachex`、`gormcachex`。

## 先读什么

| 文档 | 什么时候读 |
|---|---|
| [docs/design.md](docs/design.md) | 动任何机制之前：现行设计总览和各分篇的链接 |
| [GLOSSARY.md](GLOSSARY.md) | 写文档、写注释、和用户讨论时，用这里的叫法 |
| [docs/adr/](docs/adr/) | 想改一个「看起来很奇怪」的做法之前，先看有没有对应的决策 |
| [docs/todo.md](docs/todo.md) | 发现了但这次不解决的问题都在这里 |
| [docs/faq_ZH.md](docs/faq_ZH.md) | 用户问过的问题（英文版是 `docs/faq.md`） |
| [docs/research/](docs/research/README.md) | 实测数据；复跑脚本在 `tools/bench/` |

## 仓库布局

- 根目录是核心包 `cachex`，只依赖标准库：`cache.go`（`Cache`、`New`、选项、`Close`）、`layer.go`（层、`Layer.place` 决定一层为一个回答存什么、`Cache.answer`）、`read.go`（`Get`/`GetMany`、回源、回填、后台刷新；`fetchChunks` 按段认领，一段开始时才认领，见 ADR 0018；在途回源的结果里，不存在就是 `ErrNotFound` 本身、失败一律包过一层，所以按同一性判断不存在，数据源的错误只在 `askSource*` 分类一次；`claimed.settle` 按「发布、回填、注销」收尾，`claimed.run` 管 panic 和 Goexit）、`write.go`（`Set`/`Del`/`SetMany`/`DelMany`、失败后的失效）、`guard.go`（写入顺序守卫：对分片的全部用法，读写路径不直接碰分片）、`types.go`（接口、`ErrNotFound`、`ErrUnsupportedKey`、`BatchError`、`NewBatchError`、`Batched`）、`codec.go`（`Codec` 和内置的编解码器）、`bytes.go`（`ByteBackend`、`ByteEntry`、`Encoded`，以及只有 `Encoded` 用的私有条目编码）、`lease.go`（租约：`Leaser`/`Leasing` 及字节版、`Encoded` 的转发，以及读路径里领、用、释放租约的部分）。
- 后端子包：`ottercachex`、`bigcachex`、`rediscachex`、`gormcachex`；字节后端（`bigcachex`、`rediscachex`、`gormcachex`）只实现私有的 `ByteBackend`，`New` 返回 `cachex.Encoded` 包好的后端（`gormcachex` 外面再包一层带 `Migrate` 的 `*Backend[T]`）。`cachextest` 是测试用的内存后端（`NewLeasingMap` 是打开了租约的版本，几个 `Cache` 共用它就是多个实例）、时钟和契约测试 `TestBackend`、`TestByteBackend`、`TestLeaser`，每个后端都要过前两个，支持租约的还要过 `TestLeaser`。
- `internal/flight`（合并回源的登记表：`Claim`、只生效一次的 `Publish`、`Forget`、`Drop`）、`internal/stripe`（分片锁）：通用部件，暂不公开。
- 核心的测试是外部测试包 `cachex_test`，只通过公开接口测；`export_test.go` 只暴露两样：分片编号（`StripeIndex`，给基准测试）和 `Settle`（等后台工作结束，给读完马上查后端的测试）。测试替身用 `helpers_test.go` 的 `hooked`（单 key 和批量调用共用一个钩子），不要再为每个测试单写一个包装后端。`gormcachex` 的容器测试用 testcontainers 在 MySQL 和 PostgreSQL 上跑，没有 Docker 时自动跳过。
- `tools/bench/<日期-主题>/`：实测的复跑脚本，带 `bench` build tag，平时的 `go test ./...` 不会运行。

## 常用命令

```sh
go test -race ./...                                 # 全量，有 Docker 时约 2 分钟
go test -short -p=1 -count=1 ./...                  # CI 的跑法
golangci-lint run ./...                             # 本机和 CI 都用 v2.14.0（CI 锁在 .github/workflows/go.yml）
go test -tags bench -v -run <Test> ./tools/bench/<目录>/   # 复跑实测
```

并发相关的测试用 channel 或包装后端控制时序，不靠 `sleep` 碰运气。改了并发代码或时序测试，光跑 `-race -count=20` 不够：时序问题多半只在机器满载时出现。要在负载下压测，同时开多个进程，每个都设超时，这样卡死会以失败的形式报出来、带着 goroutine 栈：

```sh
go test -c -race -o /tmp/cachex.test .
for i in $(seq 12); do /tmp/cachex.test -test.count=30 -test.timeout=8m -test.failfast > /tmp/stress-$i.out 2>&1 || echo "FAIL $i" & done; wait
grep -h -- "--- FAIL\|test timed out" /tmp/stress-*.out
```

gormcachex 的容器测试偶尔启动失败（`port "3306/tcp" not found`，或者等 reaper 约 60 秒后超时），是 Docker Desktop 按顺序分配宿主机端口、撞上了 Mac 上某个监听 `[::]` 的进程；`startContainer` 会换端口重试，不是代码问题。

压出来的失败要查到根因：先判断是实现的问题还是测试的时序假设不对，再用人为加延迟把窗口撑大，让它稳定复现，修完再压一遍。修 bug 时先写能复现的测试，确认它在旧代码上失败。先写了实现的地方，用「故意改坏实现、确认测试会失败」补验证。性能改动用 `go test -run '^$' -bench . -benchmem -count=10` 加 benchstat 前后对比。

## 协作约定

- **文档语言**：`README.md`/`README_ZH.md`、`MIGRATION.md`/`MIGRATION_ZH.md`、`BENCHMARK.md`/`BENCHMARK_ZH.md`、`docs/faq.md`/`docs/faq_ZH.md` 是中英双语，**两份必须逐段一一对应**，改一份就同步改另一份。其余文档（`GLOSSARY.md`、`docs/` 下的全部、本文件）只写中文；术语表里保留每个术语对应的英文。
- **设计结论写回 design**：只写现在成立的设计，被推翻的直接删，理由写进 ADR。专题写进 `docs/design/` 的对应分篇，`docs/design.md` 跟着同步。
- **ADR**：只给难回退、没有背景会让人意外、确实有过取舍的决定写，编号递增。
- **不解决的问题记 todo**：当场记进 `docs/todo.md`，解决了就删。
- **用户问过的问题记 faq**：先给结论，再给依据；中英两份都要加。
- **实测存档**：报告写进 `docs/research/` 并更新索引，脚本放进 `tools/bench/<日期-主题>/`。
- **文档里不引用其他缓存库或同类项目**。依赖的库（例如 `golang.org/x/sync`、otter、BigCache）和 Go 官方的 issue 可以引用；v1 用过的 ristretto 只能作为 v1 的历史提到。
- **提交和 PR**：提交信息、PR 描述用英文，照仓库现有的风格写，不加任何 AI 署名。对使用者可见的行为或 API 变了，同步改 `MIGRATION.md`/`MIGRATION_ZH.md`。

## 底线

这些地方最容易被「顺手优化」改坏：

- **写入先写下层、再写上层，从不写数据源；某一层失败，就删掉它和它上面各层的条目。** 不要改成从上往下写。见 [ADR 0002](docs/adr/0002-bottom-up-writes.md)。
- **回填必须持分片读锁、核对写入代数，写入之后还要摘除在途回源。** 少任何一步，旧值都可能盖过写入。这些都在 `guard.go`（写入顺序守卫）里，不要在读写路径里绕过它直接用分片。回源按「发布、回填、注销」的顺序收尾，只在 `claimed.settle` 一处，见 [singleflight.md](docs/design/singleflight.md#发布与注销)、[ADR 0003](docs/adr/0003-striped-backfill-guard.md)。
- **每个在途回源恰好发布一次结果**，panic 和 Goexit 也不例外：`flight.Group.Publish` 只生效一次，`claimed.run` 在 panic 或 Goexit 时给还没发布的发布错误。不要让等待方去等一个永远不会来的结果。见 [ADR 0006](docs/adr/0006-goexit-publishes-an-error.md)。
- **刷新从不加入普通回源**，普通读取也不加入刷新（`flightKey.refresh`）；写入两种都摘除。一个 key 同时只有一次刷新，靠 `refreshing` 去重（查重不加锁，别换成认领，陈旧命中会慢）；刷新先 `track` 再认领，由这个被登记的 goroutine 回源（分段时的子 goroutine 也由它等完再注销），不能另起一个自己去登记的 goroutine，否则 `Close` 可能不等它的回填。见 [ADR 0016](docs/adr/0016-refresh-has-its-own-flights.md)。
- **租约在一层未命中之后、往下读之前领，回填只写还持有令牌的层**；开了租约的后端，每个写入都要结束 key 的租约，`PutLeased` 的「核对令牌、写入」必须是一个原子操作。没有任何层开租约时 `c.leasers` 是 nil，读路径只做 nil 检查：不要在回源 goroutine 的栈上多放东西，栈一扩容未命中就慢一倍（`putLeasedOne` 单独成函数就是为此）。见 [ADR 0020](docs/adr/0020-leases-on-shared-layers.md)。
- **回源用的 ctx 不带领头请求的取消，并标记为共享（`cachex.IsShared`）**，后端不能用它里面属于某个调用方的状态（比如事务）。核心包不认识任何后端。见 [ADR 0007](docs/adr/0007-detached-fetch-context.md)、[ADR 0013](docs/adr/0013-batch-backends-in-subpackages.md)。
- **批量读里，不存在不是错误**，缺席的 key 就是不存在；整批失败的错误要让每个 key 都失败，不能当成「全部不存在」。唯一的例外是批量数据源的错误里包着 `ErrNotFound`（整批的，或 `*BatchError` 里某个 key 的）：相应的 key 读成不存在（不缓存）。层的错误即使包着 `ErrNotFound` 也是失败（`readError`），因为后端用 false 报告不存在。见 [ADR 0004](docs/adr/0004-batch-result-shape.md)、[batch.md](docs/design/batch.md#批量调用要遵守的约定)。
- **gormcachex 的 key 列条件要用 clause 构造**（`key` 是 MySQL 的保留字），**key 列存不下的 key（超长、非法 UTF-8、含 NUL）不进 SQL**（否则整条语句失败，拖累同批的 key），**只返回 key 完全相等的行**，**批量写按字节序加锁**，MySQL 表的 `key` 列只认 `utf8mb4_0900_bin`，**只支持各数据库默认的隔离级别**、不在代码里设置它，**每条语句独立提交、从不进调用方的事务**（否则行锁跨调用持有，和分片锁死锁，见 [ADR 0017](docs/adr/0017-gorm-never-joins-a-caller-transaction.md)）。见 [ADR 0009](docs/adr/0009-gorm-exact-keys-and-collation.md)、[ADR 0010](docs/adr/0010-gorm-deadlock-ordering-and-retry.md)。
- **Redis pipeline 的错误要逐条命令判断**，连接层面的失败要设到没发出去的命令上，否则 `GET` 会把故障读成空值。见 [design/backends.md](docs/design/backends.md)。
- **字节后端解码失败要当未命中**（记 WARN、删掉条目），不能当后端故障，否则值的类型一改，读取就一直报错，直到后端让条目过期。这条规则只在 `cachex.Encoded` 里，字节后端不要自己再解码。`Encoded` 判断条目是否已经腐烂用真实时钟，不用 `Cache` 的时钟。见 [ADR 0015](docs/adr/0015-byte-backends-through-encoded.md)。
- **命中路径不分配内存**：`Get` 命中第一层时只读一次时钟和一次分片纪元，有测试用 `testing.AllocsPerRun` 断言零分配。改读路径后跑一遍 `BenchmarkGet` 对比。
