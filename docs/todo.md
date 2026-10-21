# 待办

发现了但这次不解决的问题、设计缺口。做完就从这里删掉。

## 1. go-que、go-bus 发版后换成正式版本

- **问题：** go.mod 里的 go-que 是 qor5/go-que#16（mutex 出错后重连，不再作废）的提交；go-bus 是集成分支 `molon/cachex-deps` 上的伪版本，它合了四个还没合并的 PR：qor5/go-bus#36（v0.1.2 要求被撤回的 `go-sqlite3 v2.0.3+incompatible`，会把依赖 cachex 的项目降级）、#37（依赖 go-que#16）、#38（`UnsubscribeExpired`）、#39（分批清空；发布不再用订阅前开始的订阅查询，刚订阅的实例不漏消息）。
- **做法：** go-que#16 合并发版后，go-bus#37 先换成那个版本；四个 PR 合并、go-bus 发版后，`go get github.com/qor5/go-que@<新版本> github.com/qor5/go-bus@<新版本>`，确认 `go list -m github.com/mattn/go-sqlite3` 仍是 v1.14.x，删掉这一条，也删掉 go-bus 的 `molon/cachex-deps` 分支。qor5/x 也有 go-sqlite3 的问题（qor5/x#657），cachex 不依赖它，不用跟。

## 2. 下层的答案比第一层的陈旧条目更旧时，刷新白跑

- **问题：** 第一层的条目陈旧、`CachedAt` 却比下层（共享层）那个新鲜条目晚时，每次陈旧命中都触发刷新；刷新读到下层的条目，复制时因为它更旧而跳过（ADR 0022 的比较），也不去问数据源，第一层就一直陈旧，直到腐烂，之后读到下层那个更旧的值。只在共享层被别的实例写进更旧的答案时出现（比如本实例领不到租约、别的实例回填了更早读到的值），属于 ADR 0022 不保证的跨实例范围，代价是每次陈旧命中多读一次下层。
- **做法：** 复制被跳过时，让这次刷新接着问数据源（同一个 key 只问一次的规则照旧）。要先写出能复现的测试。

## 3. `Close` 之后放弃等待的写入，补做的删除没人等

- **问题：** `Close` 之后 `Cache` 仍可写；这时一个写入等分片超时放弃，补做的删除在一个不受追踪的 goroutine 里跑，可能在调用方关闭后端之后才碰后端。`Close` 的文档只承诺等它之前安排的。
同类：gobuscachex 的处理函数跑在 go-que 的 worker goroutine 上，`consumer.Stop` 最多等 10 秒，一个卡在 `invalidateLocal` 里的处理（最长一个回源超时）可能比 `Listen`、也就比 `Close` 活得久（未用容器验证）。
- **做法：** 低优先级。要么 `Close` 之后的写入不再等分片（直接失败并当场删除），要么文档写明。
