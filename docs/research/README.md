# 实测与调研

| 报告 | 结论 | 复跑脚本 |
|---|---|---|
| [2026-10-gorm-deadlock.md](2026-10-gorm-deadlock.md) | 统一加锁顺序加有限重试，在 MySQL 和 PostgreSQL 上都消除了并发批量写的死锁；严格的锁表也能做到，但吞吐低 41% 到 50%；复跑脚本自带断言，完整负载验证过两遍 | [tools/bench/2026-10-gorm-deadlock](../../tools/bench/2026-10-gorm-deadlock/) |
| [2026-10-double-check.md](2026-10-double-check.md) | 二次检查只在后端读取有延迟、key 又很热时有收益；按需二次检查在热点 key 上拿到几乎全部收益，在冷 key 上几乎零成本 | [tools/bench/2026-10-double-check](../../tools/bench/2026-10-double-check/) |
| [2026-10-micro-benchmarks.md](2026-10-micro-benchmarks.md) | cachex 自身开销的基线：新鲜命中约 17ns、0 次分配；去掉 pkg/errors 后，经过「不存在」或回源的路径快 20% 到 67%；`GetMany` 全命中时每 key 比循环 `Get` 慢 4 到 7 倍，有未命中时快 1.5 到 3.4 倍 | `benchmark_test.go` |
| [2026-10-batch-result-shape.md](2026-10-batch-result-shape.md) | 批量读的三种返回形态里，按 key 顺序排列的切片最快，`map[string]Result` 最慢；和一次后端往返相比，差别可以忽略 | [tools/bench/2026-10-batch-result-shape](../../tools/bench/2026-10-batch-result-shape/) |

`tools/bench/` 下的复跑脚本都带 `bench` 这个 build tag，平时的 `go test ./...` 不会运行它们。复跑命令写在各报告和脚本的开头。
