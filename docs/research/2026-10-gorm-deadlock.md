# GORMCache 并发批量写的死锁

日期：2026-10-09。对应决策：[ADR 0010](../adr/0010-gorm-deadlock-ordering-and-retry.md)。

## 问题

多个实例同时用 `GetMany` 回填有重叠的 key 时，`GORMCache.SetMany`/`DelMany` 会在数据库里死锁。被数据库中止的那次回填只记一条 WARN 日志，这些 key 就一直没能缓存上；PostgreSQL 每次死锁还要先等 `deadlock_timeout`（默认 1 秒）才能检测出来，期间一直占着行锁和 cachex 的分片。

死锁有两个来源：

1. `SetMany` 按 Go map 的随机顺序生成多行 upsert。两条语句以相反的顺序锁同样的几行。
2. PostgreSQL 的 `DELETE … WHERE key IN (…)` 按索引顺序锁行，而索引顺序跟着列的排序规则走（官方镜像默认是 `en_US.utf8`），和 `SetMany` 排序用的字节序不一致。key 里大小写混排时（`a000`、`B001`……），两者的顺序正好相反。

## 方法

- 用 testcontainers 起 MySQL 8.4 和 PostgreSQL 16（默认配置，PostgreSQL 的数据库排序规则是 `en_US.utf8`）。
- key 大小写混排，让字节序和 `en_US` 的顺序不同。
- N 个 worker 并发，每轮从 key 空间里随机取 100 个 key，60% 的概率 `SetMany`、40% 的概率 `DelMany`。
- 统计被数据库判为死锁的操作次数和吞吐量。「锁表」用 MySQL 的 `GET_LOCK` 或 PostgreSQL 的 `pg_advisory_xact_lock` 让同一张表的写入串行执行，效果等同于锁表。

## 结果

### 第一轮：8 个 worker × 40 轮，400 个 key

这一轮的「现状」是当时的代码：`SetMany` 已经排序，`DelMany` 是普通的 `DELETE … IN`。

| 方案 | MySQL 死锁 | MySQL 吞吐 | PostgreSQL 死锁 | PostgreSQL 吞吐 |
|---|---|---|---|---|
| 现状 | 2 / 320 | 890 ops/s | **149 / 320** | **2 ops/s**（140 秒） |
| 只重试 | 0 | 892 | 51 / 320 | 1 ops/s（460 秒） |
| `DELETE` 也按字节序加锁 | 4 / 320 | 893 | **0** | **1566** |
| MySQL 改用 READ COMMITTED | 8 / 320 | 847 | — | — |
| 锁表 | 0 | 728 | 0 | 1227 |

- PostgreSQL 的死锁绝大多数来自来源 2。让 `DELETE` 按字节序加锁后直接归零，吞吐也是所有方案里最高的。
- 只重试，在 PostgreSQL 上反而更糟：重来一遍还会撞上。
- MySQL 上即使加锁顺序统一，仍有少量死锁，来自间隙锁和插入意向锁的交互。改用 READ COMMITTED 反而更多。InnoDB 的文档要求应用对这类死锁自行重试。

### 第二轮：16 个 worker × 80 轮，2000 个 key

| 方案 | MySQL | PostgreSQL |
|---|---|---|
| 统一加锁顺序 | 3 次死锁 / 1280，949 ops/s | 0，1922 ops/s |
| **统一加锁顺序 + 重试（采用）** | **0，840 ops/s** | **0，1936 ops/s** |
| 锁表 | 0，647 ops/s（-32%） | 0，1045 ops/s（-46%） |

锁表的吞吐损失随并发上升：8 个 worker 时约 -20%，16 个时 -32% 到 -46%。缓存层正是高并发的地方，所以没有采用。

这两轮实验里，MySQL 的「锁表」在事务提交之前就释放了命名锁，锁得不够严格；严格的锁表吞吐只会更低，结论不变。复跑脚本已经改成提交之后才释放。

### 复跑脚本的抽样结果

`tools/bench/2026-10-gorm-deadlock` 把上面的方案整理成了四种：`unordered` 是修复之前的写法（随机顺序 upsert，普通 `DELETE`）；`ordered` 是统一加锁顺序但不重试；`ordered+retry` 直接调用现在的 `GORMCache`；`table lock` 是锁表。用很小的负载（8 个 worker × 10 轮）验证过脚本：

| 方案 | MySQL 死锁 | PostgreSQL 死锁 |
|---|---|---|
| unordered | 52 / 80 | 30 / 80（24 秒） |
| ordered | 0 / 80 | 0 / 80 |
| ordered+retry（GORMCache） | 0 / 80 | 0 / 80 |
| table lock | 0 / 80 | 0 / 80 |

这么小的负载下，吞吐量没有参考意义；比较吞吐请用默认负载复跑。脚本只复现第二轮的设置（2000 个 key），第一轮的「只重试」和「READ COMMITTED」两种方案没有放进脚本。

## 复跑

```sh
# 需要 Docker。默认 16 个 worker × 80 轮；unordered 在 PostgreSQL 上很慢（每次死锁都要等 1 秒）
go test -tags bench -run TestDeadlockStrategies -v -timeout 60m ./tools/bench/2026-10-gorm-deadlock/
WORKERS=8 ROUNDS=10 go test -tags bench -run TestDeadlockStrategies -v ./tools/bench/2026-10-gorm-deadlock/
```
