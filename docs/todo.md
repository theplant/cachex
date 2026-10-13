# 待办

发现了但这次不解决的问题、设计缺口。做完就从这里删掉。

## 1. go-bus 发版后换成正式版本

- **问题：** go.mod 里的 go-bus 是 PR 分支上的伪版本（`v0.1.3-0.20261012191021-fea608be7176`，qor5/go-bus#36），因为 v0.1.2 要求被撤回的 `go-sqlite3 v2.0.3+incompatible`，会把依赖 cachex 的项目降级（见 [backends.md](design/backends.md#gobuscachex)）。
- **做法：** PR 合并、go-bus 发版后，`go get github.com/qor5/go-bus@<新版本>`，确认 `go list -m github.com/mattn/go-sqlite3` 仍是 v1.14.x，删掉这一条。qor5/x 也有同样的问题（qor5/x#657），cachex 不依赖它，不用跟。

## 2. go-que 的 mutex 断线后不能恢复：上游修好后删掉两处变通

- **问题：** go-que 的 `pg.mutex` 把第一次 `Lock`/`Unlock` 的错误存进 `m.err`、永不清除，之后每次都是 `que: bad mutex`；go-bus 的 quex 重建 worker 时复用同一份 `WorkerOptions`（同一个 mutex），所以数据库连接断一次（重启、主从切换），worker 就永远失败。cachex 里两处受影响，都做了变通：`goquecachex` 的 `renewingMutex`（坏了就换新的），`gobuscachex` 的探测投递（停投一个 TTL 就重新订阅）。
- **做法：** 给 go-bus 提 PR，让 quex 在重建 worker 时换新的 mutex（比如 `StartWorkerOption` 接收一个 mutex 工厂，`bus.go` 传 `GoQue().Mutex`），或者给 go-que 提 PR 让 mutex 在出错后重新取连接。合并发版后删掉 `renewingMutex`；探测投递可以留着兜底别的停投，也可以删，到时再定。

## 3. 下层的答案比第一层的陈旧条目更旧时，刷新白跑

- **问题：** 第一层的条目陈旧、`CachedAt` 却比下层（共享层）那个新鲜条目晚时，每次陈旧命中都触发刷新；刷新读到下层的条目，复制时因为它更旧而跳过（ADR 0022 的比较），也不去问数据源，第一层就一直陈旧，直到腐烂，之后读到下层那个更旧的值。只在共享层被别的实例写进更旧的答案时出现（比如本实例领不到租约、别的实例回填了更早读到的值），属于 ADR 0022 不保证的跨实例范围，代价是每次陈旧命中多读一次下层。
- **做法：** 复制被跳过时，让这次刷新接着问数据源（同一个 key 只问一次的规则照旧）。要先写出能复现的测试。

## 4. `Close` 之后放弃等待的写入，补做的删除没人等

- **问题：** `Close` 之后 `Cache` 仍可写；这时一个写入等分片超时放弃，补做的删除在一个不受追踪的 goroutine 里跑，可能在调用方关闭后端之后才碰后端。`Close` 的文档只承诺等它之前安排的。
同类：gobuscachex 的处理函数跑在 go-que 的 worker goroutine 上，`consumer.Stop` 最多等 10 秒，一个卡在 `invalidateLocal` 里的处理（最长一个回源超时）可能比 `Listen`、也就比 `Close` 活得久（未用容器验证）。
- **做法：** 低优先级。要么 `Close` 之后的写入不再等分片（直接失败并当场删除），要么文档写明。

## 5. go-bus 不回收过期订阅的队列，也不清重试过的任务：崩溃的实例会在 `goque_jobs` 里留下行

- **问题：** gobuscachex 每次 `Listen` 用一个随机名字的队列，正常退出时退订（软删除，加 auto-drain）。但实例崩溃（SIGKILL、OOM、节点丢了）时没人退订：订阅在 TTL 内继续收消息，过期后 go-bus 既不软删除它，也不清它的队列，所以每次崩溃最多留下一个 TTL 的消息（默认 1 分钟乘以写入速率）和两条订阅行。另外，`Destroy` 失败过一次的任务带着 `retry_count > 0`，go-bus 的 auto-drain（`retry_count = 0` 才删）跳过它，队列退订后它就永远留下（数据库主从切换时最容易出现）。这些都只是行变多，不影响正确性：失效是幂等的。
- **做法：** 上游修：go-bus 回收过期订阅（软删除并清队列），drain 也删重试过的、没人在跑的任务。cachex 这边不直接写 go-bus 的表（耦合它的内部结构）。上游合并发版之前，可以定期执行 `DELETE FROM goque_jobs WHERE queue LIKE 'cachex-%' AND queue NOT LIKE 'cachex-cleanup:%' AND queue NOT IN (SELECT queue FROM gobus_subscriptions WHERE deleted_at IS NULL AND (expires_at IS NULL OR expires_at > NOW()))` 兜底（`cachex-cleanup:` 是 goquecachex 的清理任务，不能删；先确认这个语句在当时的 go-bus 版本上仍然成立）。

## 6. 复制看不到它读来源层之后的变化，读到的值可能倒退

- **问题：** 复制从下层读到条目、到拿着回填锁落下之间，`land` 只读它要写的那些层（来源层之上）；来源层之后的变化它看不到。随机压力测试（很多层、随机淘汰、共用分片、租约）找到几种情形，`knowngaps_test.go` 里有确定性复现（去掉 skip 就失败）：
  - 两层，第一层先后被淘汰两次：复制拿着较早的 v1 落下，而来源层其实已经是 v2；
  - 数据源删了这个 key（读到不存在、默认不记录），复制随后把旧值 v1 落进第一层，整个新鲜期都返回它；
  - 不淘汰、四层：一个在落下前腐烂的复制把上层更长寿的同一答案删掉，另一个更旧的复制随后落下（只在压力日志里见过）；
  - 租约层在上、它的租约被这次复制持有时，更新的答案写不进去（只在压力日志里见过）。
  都违反 ADR 0022；单进程、无写入也会发生，但要淘汰、腐烂或数据源删除恰好卡在窗口里。
- **做法（要改设计，先和用户商量）：** 记下复制读来源层时的分片纪元；落下时纪元变了就重读来源层，条目变了或没了就作废这次复制（不落、不发布），回源流程从下一层或数据源继续；新的腐烂条目也算「更新」。这要让 `settle`/回填能把一个 key 交回给回源流程。
