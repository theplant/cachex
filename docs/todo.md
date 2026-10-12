# 待办

发现了但这次不解决的问题、设计缺口。做完就从这里删掉。

## 1. 多实例一致性

分片锁只在一个 Cache 所在的进程内保证顺序。多个实例共用 Redis 等层时，下面两点目前只靠 TTL 兜底，见 [design/consistency.md](design/consistency.md) 和 [ADR 0011](adr/0011-cross-instance-consistency-via-ttl.md)。

### 1.1 跨实例的回源去重

- **问题：** 部署 20 个实例时，每个实例的合并回源只在自己进程内生效。一个热点 key 腐烂，数据源会被回源 20 次。
- **做法：** 在租约（[consistency.md](design/consistency.md#租约lease)）的基础上，拿不到租约的普通读取不立即回源，而是等一小段时间再读共享层，超时再自己回源。后台刷新已经会在拿不到租约时跳过数据源。
- **要定的事：** 等多久、怎么等（轮询还是订阅）、超时后的退路；领租约要变成「读和领在同一个原子操作里」，否则两个实例可能都以为自己该回源。

## 2. go-bus 发版后换成正式版本

- **问题：** go.mod 里的 go-bus 是 PR 分支上的伪版本（`v0.1.3-0.20261012191021-fea608be7176`，qor5/go-bus#36），因为 v0.1.2 要求被撤回的 `go-sqlite3 v2.0.3+incompatible`，会把依赖 cachex 的项目降级（见 [backends.md](design/backends.md#gobuscachex)）。
- **做法：** PR 合并、go-bus 发版后，`go get github.com/qor5/go-bus@<新版本>`，确认 `go list -m github.com/mattn/go-sqlite3` 仍是 v1.14.x，删掉这一条。qor5/x 也有同样的问题（qor5/x#657），cachex 不依赖它，不用跟。

## 3. gormcachex 清理腐烂的行

- **问题：** 表只增不减，过了 `expires_at` 的行要用户自己删。
- **做法：** 提供一个可选的清理函数或后台任务：按 `expires_at < now` 分小批删除（例如每批 100 行、批间停顿），可配置间隔，随 Close 停止；需要时加监控指标。`expires_at` 列已经有索引。

## 4. 热度统计与预热

- **问题：** 重启后内存层是空的，第一波请求都要回源；也没有办法知道哪些 key 最热。
- **做法：** 先做热度统计（每个 key 的访问次数和最后访问时间，带衰减），在此基础上：
  - 关闭时导出最热的 N 个 key，启动时用 `GetMany` 预热；
- **注意：** 都做成可选功能；先加基准测试量化统计本身的开销，再决定是否默认开启。
