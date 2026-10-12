# 待办

发现了但这次不解决的问题、设计缺口。做完就从这里删掉。

## 1. go-bus 发版后换成正式版本

- **问题：** go.mod 里的 go-bus 是 PR 分支上的伪版本（`v0.1.3-0.20261012191021-fea608be7176`，qor5/go-bus#36），因为 v0.1.2 要求被撤回的 `go-sqlite3 v2.0.3+incompatible`，会把依赖 cachex 的项目降级（见 [backends.md](design/backends.md#gobuscachex)）。
- **做法：** PR 合并、go-bus 发版后，`go get github.com/qor5/go-bus@<新版本>`，确认 `go list -m github.com/mattn/go-sqlite3` 仍是 v1.14.x，删掉这一条。qor5/x 也有同样的问题（qor5/x#657），cachex 不依赖它，不用跟。
