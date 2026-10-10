# 实测与调研

| 报告 | 结论 | 复跑脚本 |
|---|---|---|
| [2026-10-gorm-deadlock.md](2026-10-gorm-deadlock.md) | 统一加锁顺序加有限重试，在 MySQL 和 PostgreSQL 上都消除了并发批量写的死锁；锁表也能做到，但吞吐低 32% 到 46% | [tools/bench/2026-10-gorm-deadlock](../../tools/bench/2026-10-gorm-deadlock/) |
| [2026-10-double-check.md](2026-10-double-check.md) | 二次检查只在后端读取有延迟、key 又很热时有收益；按需二次检查在热点 key 上拿到几乎全部收益，在冷 key 上几乎零成本 | [tools/bench/2026-10-double-check](../../tools/bench/2026-10-double-check/) |
| [2026-10-batch-result-shape.md](2026-10-batch-result-shape.md) | 批量读的三种返回形态里，按 key 顺序排列的切片最快，`map[string]Result` 最慢；和一次后端往返相比，差别可以忽略 | [tools/bench/2026-10-batch-result-shape](../../tools/bench/2026-10-batch-result-shape/) |

复跑脚本都带 `bench` 这个 build tag，平时的 `go test ./...` 不会运行它们。复跑命令写在各报告和脚本的开头。
