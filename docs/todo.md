# 待办

发现了但这次不解决的问题、设计缺口。做完就从这里删掉。

## 1. go-bus 发版后换成正式版本

- **问题：** go.mod 里的 go-bus 是 PR 分支上的伪版本（`v0.1.3-0.20261012191021-fea608be7176`，qor5/go-bus#36），因为 v0.1.2 要求被撤回的 `go-sqlite3 v2.0.3+incompatible`，会把依赖 cachex 的项目降级（见 [backends.md](design/backends.md#gobuscachex)）。
- **做法：** PR 合并、go-bus 发版后，`go get github.com/qor5/go-bus@<新版本>`，确认 `go list -m github.com/mattn/go-sqlite3` 仍是 v1.14.x，删掉这一条。qor5/x 也有同样的问题（qor5/x#657），cachex 不依赖它，不用跟。

## 2. go-que 的 mutex 断线后不能恢复：上游修好后删掉两处变通

- **问题：** go-que 的 `pg.mutex` 把第一次 `Lock`/`Unlock` 的错误存进 `m.err`、永不清除，之后每次都是 `que: bad mutex`；go-bus 的 quex 重建 worker 时复用同一份 `WorkerOptions`（同一个 mutex），所以数据库连接断一次（重启、主从切换），worker 就永远失败。cachex 里两处受影响，都做了变通：`goquecachex` 的 `renewingMutex`（坏了就换新的），`gobuscachex` 的探测投递（停投一个 TTL 就重新订阅）。
- **做法：** 给 go-bus 提 PR，让 quex 在重建 worker 时换新的 mutex（比如 `StartWorkerOption` 接收一个 mutex 工厂，`bus.go` 传 `GoQue().Mutex`），或者给 go-que 提 PR 让 mutex 在出错后重新取连接。合并发版后删掉 `renewingMutex`；探测投递可以留着兜底别的停投，也可以删，到时再定。
