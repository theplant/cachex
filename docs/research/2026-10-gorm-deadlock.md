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

这两轮实验里，MySQL 的「锁表」在事务提交之前就释放了命名锁，锁得不够严格。复跑脚本已经改成提交之后才释放，严格锁表的结果见下一节。

### 复跑脚本的完整验证（2026-10-10）

`tools/bench/2026-10-gorm-deadlock` 把上面的方案整理成四种：

| 策略 | 是什么 | 脚本断言 |
|---|---|---|
| `unordered` | 修复之前的写法：随机顺序 upsert，普通 `DELETE` | **必须**复现出死锁，否则说明负载没有触发竞态，实验无效 |
| `ordered` | 统一加锁顺序，不重试 | PostgreSQL 上必须零死锁（MySQL 的间隙锁允许少量死锁） |
| `ordered+retry` | 直接调用 v1 的 `GORMCache`（v2 起是 `gormcachex`） | 零死锁、零错误传到调用方 |
| `table lock` | 整表串行写：MySQL 在同一个连接上，于事务开始前取命名锁、提交后释放；PostgreSQL 用事务级咨询锁 | 零死锁、零错误，并且锁内同一时刻最多只有 1 个写入 |

每种策略还会记录一条真实的死锁错误样本，确认计数的确实是数据库的死锁（MySQL 是 `Error 1213 (40001): Deadlock found…`，PostgreSQL 是 `ERROR: deadlock detected (SQLSTATE 40P01)`）。

**断言本身也做过反向验证**：
- 把「锁表」改成不加锁，断言报告「8 writes ran inside the lock at once」，测试失败；
- 把 `GORMCache.SetMany` 的排序去掉（脚本引用的是本地的库代码），断言报告 PostgreSQL 上「22 deadlocks … reached the caller」，测试失败。MySQL 上的死锁全部被重试吸收，这也说明重试只能兜住少量死锁，PostgreSQL 必须靠统一加锁顺序。

默认负载（16 个 worker × 80 轮，2000 个 key）完整跑了两遍，断言全部通过：

| 策略 | MySQL 第 1 遍 | MySQL 第 2 遍 | PostgreSQL 第 1 遍 | PostgreSQL 第 2 遍 |
|---|---|---|---|---|
| unordered | 1072 次死锁 | 1052 次死锁 | 781 次死锁（646 秒） | 747 次死锁（620 秒） |
| ordered | 11 次死锁，796 ops/s | 8 次死锁，607 ops/s | 0，1326 ops/s | 0，1516 ops/s |
| ordered+retry（v1 GORMCache） | **0，794 ops/s** | **0，533 ops/s** | **0，1561 ops/s** | **0，1613 ops/s** |
| table lock | 0，462 ops/s（-42%） | 0，315 ops/s（-41%） | 0，787 ops/s（-50%） | 0，833 ops/s（-48%） |

- 第 2 遍运行时，机器上同时在跑其他测试，MySQL 的绝对吞吐偏低；各策略之间的相对关系不变。
- 严格的锁表比前两轮测到的更慢（-41% 到 -50%，前两轮是 -32% 到 -46%），「不锁表」的结论更站得住。
- `unordered` 在 MySQL 上看起来吞吐很高，是因为大部分操作直接以死锁失败返回了。

脚本只复现第二轮的设置（2000 个 key）；第一轮的「只重试」和「READ COMMITTED」两种方案没有放进脚本。

### v2 的 gormcachex（2026-10-10）

v2 把后端拆成 `gormcachex`，表多了 `expires_at` 列、值改成编码后的字节，加锁顺序和重试不变。用同一个脚本、同样的负载跑了一遍，断言全部通过：

| 策略 | MySQL 8.4 | PostgreSQL 16 |
|---|---|---|
| unordered | 1075 次死锁 | 746 次死锁（657 秒） |
| ordered | 5 次死锁，360 ops/s | 0，1354 ops/s |
| **ordered+retry（gormcachex）** | **0，797 ops/s** | **0，1115 ops/s** |
| table lock | 0，396 ops/s（-50%） | 0，855 ops/s（-23%） |

和 v1 的结论一致：统一加锁顺序加有限重试，两个库上都没有死锁传到调用方，吞吐和 v1 相当；锁表的损失仍然明显。这一遍跑在本机同时有其他测试运行的时候，PostgreSQL 的锁表损失比前面小，绝对数字只作参考。

## 复跑

```sh
# 需要 Docker。默认 16 个 worker × 80 轮；unordered 在 PostgreSQL 上很慢（每次死锁都要等 1 秒）
go test -tags bench -run TestDeadlockStrategies -v -timeout 60m ./tools/bench/2026-10-gorm-deadlock/
WORKERS=8 ROUNDS=10 go test -tags bench -run TestDeadlockStrategies -v ./tools/bench/2026-10-gorm-deadlock/
```
