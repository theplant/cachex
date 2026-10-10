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

- 根目录是核心包 `cachex`，只依赖标准库：`cache.go`（`Cache`、`New`、选项、`Close`）、`layer.go`（层、条目的寿命和状态）、`read.go`（`Get`/`GetMany`、回源、回填、后台刷新、`claimed`：每个在途回源恰好发布一次）、`write.go`（`Set`/`Del`/`SetMany`/`DelMany`）、`types.go`（接口、`ErrNotFound`、`BatchError`）、`codec.go`（`Codec`、条目编码、`Batched`）。
- 后端子包：`ottercachex`、`bigcachex`、`rediscachex`、`gormcachex`；`cachextest` 是测试用的内存后端、时钟和后端契约测试 `TestBackend`，每个后端都要过它。
- `internal/flight`（合并回源的登记表）、`internal/stripe`（分片锁）：通用部件，暂不公开。
- 核心的测试是外部测试包 `cachex_test`，只通过公开接口测；`export_test.go` 只给基准测试暴露分片编号。`gormcachex` 的容器测试用 testcontainers 在 MySQL 和 PostgreSQL 上跑，没有 Docker 时自动跳过。
- `tools/bench/<日期-主题>/`：实测的复跑脚本，带 `bench` build tag，平时的 `go test ./...` 不会运行。

## 常用命令

```sh
go test -race ./...                                 # 全量，有 Docker 时约 2 分钟
go test -short -p=1 -count=1 ./...                  # CI 的跑法
golangci-lint run ./...                             # 本机和 CI 都用 v2.14.0（CI 锁在 .github/workflows/go.yml）
go test -tags bench -v -run <Test> ./tools/bench/<目录>/   # 复跑实测
```

并发相关的测试用 channel 或包装后端控制时序，不靠 `sleep` 碰运气；新加的时序测试用 `-race -count=20` 以上跑几遍确认稳定。修 bug 时先写能复现的测试，确认它在旧代码上失败。先写了实现的地方，用「故意改坏实现、确认测试会失败」补验证。性能改动用 `go test -run '^$' -bench . -benchmem -count=10` 加 benchstat 前后对比。

## 协作约定

- **文档语言**：`README.md`/`README_ZH.md`、`MIGRATION.md`/`MIGRATION_ZH.md`、`BENCHMARK.md`/`BENCHMARK_ZH.md`、`docs/faq.md`/`docs/faq_ZH.md` 是中英双语，**两份必须逐段一一对应**，改一份就同步改另一份。其余文档（`GLOSSARY.md`、`docs/` 下的全部、本文件）只写中文；术语表里保留每个术语对应的英文。
- **设计结论写回 design**：只写现在成立的设计，被推翻的直接删，理由写进 ADR。专题写进 `docs/design/` 的对应分篇，`docs/design.md` 跟着同步。
- **ADR**：只给难回退、没有背景会让人意外、确实有过取舍的决定写，编号递增。
- **不解决的问题记 todo**：当场记进 `docs/todo.md`，解决了就删。
- **用户问过的问题记 faq**：先给结论，再给依据；中英两份都要加。
- **实测存档**：报告写进 `docs/research/` 并更新索引，脚本放进 `tools/bench/<日期-主题>/`。
- **文档里不引用其他缓存库或同类项目**。依赖的库（例如 `golang.org/x/sync`）和 Go 官方的 issue 可以引用。
- **提交和 PR**：提交信息、PR 描述用英文，照仓库现有的风格写，不加任何 AI 署名。对使用者可见的行为或 API 变了，同步改 `MIGRATION.md`/`MIGRATION_ZH.md`。

## 底线

这些地方最容易被「顺手优化」改坏：

- **写入先写下层、再写上层，从不写数据源；某一层失败，就删掉它和它上面各层的条目。** 不要改成从上往下写。见 [ADR 0002](docs/adr/0002-upstream-first-writes.md)。
- **回填必须持分片读锁、核对写入代数，写入之后还要摘除在途回源。** 少任何一步，旧值都可能盖过写入。回源先发布结果、回填完成后才注销在途回源，这期间来的读取加入它，不会再回源。见 [ADR 0003](docs/adr/0003-striped-backfill-guard.md)。
- **每个在途回源恰好发布一次结果**，panic 和 Goexit 也不例外。不要让等待方去等一个永远不会来的结果。见 [ADR 0006](docs/adr/0006-goexit-publishes-an-error.md)。
- **回源用的 ctx 不带领头请求的取消，并标记为共享（`cachex.IsShared`）**，后端不能用它里面属于某个调用方的状态（比如 `gormcachex` 的事务）。核心包不认识任何后端。见 [ADR 0007](docs/adr/0007-detached-fetch-context.md)、[ADR 0013](docs/adr/0013-batch-backends-in-subpackages.md)。
- **批量读里，不存在不是错误**，缺席的 key 就是不存在；整批失败的错误不能被当成「全部不存在」。见 [ADR 0004](docs/adr/0004-batch-result-shape.md)。
- **gormcachex 的 key 列条件要用 clause 构造**（`key` 是 MySQL 的保留字），**只返回 key 完全相等的行**，**批量写按字节序加锁**，MySQL 表的 `key` 列只认 `utf8mb4_0900_bin`，**只支持各数据库默认的隔离级别**、不在代码里设置它。见 [ADR 0009](docs/adr/0009-gorm-exact-keys-and-collation.md)、[ADR 0010](docs/adr/0010-gorm-deadlock-ordering-and-retry.md)。
- **Redis pipeline 的错误要逐条命令判断**，连接层面的失败要设到没发出去的命令上，否则 `GET` 会把故障读成空值。见 [design/backends.md](docs/design/backends.md)。
- **字节后端解码失败要当未命中**（记 WARN、删掉条目），不能当后端故障，否则值的类型一改，读取就一直报错到条目过期。见 [ADR 0013](docs/adr/0013-batch-backends-in-subpackages.md)。
- **命中路径不分配内存**：`Get` 命中第一层时只读一次时钟和一次分片纪元。改读路径后跑一遍 `BenchmarkGet` 对比。
