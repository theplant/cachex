# 用自己的合并回源实现（internal/flight）替换 x/sync/singleflight

`GetMany` 要在同一套合并回源里**逐个认领**多个 key，然后把自己领头的那些 key 合成一次批量调用。认领时必须**当场**知道自己是不是领头请求。`golang.org/x/sync/singleflight` 的 `DoChan` 做不到：它在锁内决定谁是领头，但把回源函数放进另一个 goroutine 执行，不把结果告诉调用方。如果为了得到答案而等待，两个互相重叠的批量读会各自等对方手里的 key，从而死锁。所以我们写了 `flight.Group`（内部包 `internal/flight`）：`Claim` 立即返回（在途回源，是否领头），`Finish` 负责发布。`Get` 和 `GetMany` 共用它，因此一个 `Get` 和一个包含同一 key 的 `GetMany` 也只回源一次。

## 考虑过的方案

- **继续用 x/sync，`GetMany` 自己维护一张登记表**：`Get` 和 `GetMany` 之间就不能合并回源，同一个 key 会被回源两次。
- **整批作为一个 singleflight key**：同一批 key 才能合并，只要有一个 key 不同就要重新回源，也无法和 `Get` 合并。

## 后果

- 单 key 的行为逐项对照过 main：合并方式、回源槽位、panic 的错误信息和日志、调用方取消、超时、测试钩子都保持一致。有一个**有意的偏离**：Goexit 时会给等待方发布错误，见 [ADR 0006](0006-goexit-publishes-an-error.md)。`get_compat_test.go` 固定了单 key 的这些可观察行为。
- `Finish` 先移除登记、再发布结果，和 x/sync 的顺序相同。现有的二次检查依赖这个顺序。
- `x/sync` 仍然留在 `go.mod` 里，基准测试用到了它的 semaphore。
