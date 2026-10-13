# 待办

发现了但这次不解决的问题、设计缺口。做完就从这里删掉。

## 1. go-bus 发版后换成正式版本

- **问题：** go.mod 里的 go-bus 是 PR 分支上的伪版本（`v0.1.3-0.20261012191021-fea608be7176`，qor5/go-bus#36），因为 v0.1.2 要求被撤回的 `go-sqlite3 v2.0.3+incompatible`，会把依赖 cachex 的项目降级（见 [backends.md](design/backends.md#gobuscachex)）。
- **做法：** PR 合并、go-bus 发版后，`go get github.com/qor5/go-bus@<新版本>`，确认 `go list -m github.com/mattn/go-sqlite3` 仍是 v1.14.x，删掉这一条。qor5/x 也有同样的问题（qor5/x#657），cachex 不依赖它，不用跟。

## 2. go-que 的 mutex 断线后作废、quex 却复用它：上游修好后删掉两处变通

- **问题：** go-que 的 `pg.mutex` 出任何错都会作废，之后每次都是 `que: bad mutex`；这是 go-que 有意的设计（`TestErrUnlockedJobs` 断言了它），用法是新建 worker 时配一个新的 `q.Mutex()`。但 qor5/go-bus 的 quex 重建 worker 时复用了同一个 mutex，所以数据库连接断一次（重启、主从切换），worker 就永远失败。cachex 里两处受影响，都做了变通：`goquecachex` 的 `renewingMutex`（坏了就换新的），`gobuscachex` 的探测投递（停投一个 TTL 就重新订阅）。
- **做法：** 修在 qor5/go-bus（本地分支 `molon/quex-fresh-mutex`，提交 0db16bb，待用户确认后开 PR）：quex 新增可选的 `WithMutexFactory`，每次创建或重建 worker 都取一个新 mutex，`StartConsumer` 传入 `GoQue().Mutex`。发版后：`goquecachex` 改为传 `quex.WithMutexFactory(q.Mutex)`，删掉 `renewingMutex`；`gobuscachex` 删掉探测投递（修好之后已知的停投原因只剩 mutex 那条连接半开，TCP keepalive 几分钟内会报错，接着就能恢复；心跳和 `consumer.Done()` 覆盖其他情形）。

## 3. 下层的答案比第一层的陈旧条目更旧时，刷新白跑

- **问题：** 第一层的条目陈旧、`CachedAt` 却比下层（共享层）那个新鲜条目晚时，每次陈旧命中都触发刷新；刷新读到下层的条目，复制时因为它更旧而跳过（ADR 0022 的比较），也不去问数据源，第一层就一直陈旧，直到腐烂，之后读到下层那个更旧的值。只在共享层被别的实例写进更旧的答案时出现（比如本实例领不到租约、别的实例回填了更早读到的值），属于 ADR 0022 不保证的跨实例范围，代价是每次陈旧命中多读一次下层。
- **做法：** 复制被跳过时，让这次刷新接着问数据源（同一个 key 只问一次的规则照旧）。要先写出能复现的测试。

## 4. `Close` 之后放弃等待的写入，补做的删除没人等

- **问题：** `Close` 之后 `Cache` 仍可写；这时一个写入等分片超时放弃，补做的删除在一个不受追踪的 goroutine 里跑，可能在调用方关闭后端之后才碰后端。`Close` 的文档只承诺等它之前安排的。
同类：gobuscachex 的处理函数跑在 go-que 的 worker goroutine 上，`consumer.Stop` 最多等 10 秒，一个卡在 `invalidateLocal` 里的处理（最长一个回源超时）可能比 `Listen`、也就比 `Close` 活得久（未用容器验证）。
- **做法：** 低优先级。要么 `Close` 之后的写入不再等分片（直接失败并当场删除），要么文档写明。

## 5. go-bus 不回收崩溃实例的订阅：它们的队列在 `goque_jobs` 里留下行

- **问题：** gobuscachex 每次 `Listen` 用一个随机名字的队列，正常退出时退订（软删除，开了 `AutoDrain` 就清队列）。实例崩溃（SIGKILL、OOM、节点丢了）时没人退订：订阅在 TTL 内继续收消息，过期后 go-bus 不再投递，但订阅行和队列里的积压都留着，每次崩溃最多留下一个 TTL 的消息（默认 1 分钟乘以写入速率）和两条订阅行。只是行变多，不影响正确性。`Destroy` 失败过、带着 `retry_count > 0` 的任务退订时本来就不清（go-bus 有意保留失败过的任务），不算这个问题。
- **做法：** qor5/go-bus 本地分支 `molon/unsubscribe-expired`（提交 d7f7be3，待用户确认后开 PR），新增 `BusImpl.UnsubscribeExpired(ctx)`：找出过期、还没删除的订阅，逐个走现有的退订逻辑（清不清队列按各自的 `AutoDrain`，重试过的照样保留），分批处理（每次限量、每批一个短事务，删任务也按批），避免一次锁太多行、给数据库压力，返回退订了几个。不加订阅选项，现有方法行为不变。发版后 gobuscachex 在心跳循环里调用它（失败只记日志），替崩溃的同伴退订。
