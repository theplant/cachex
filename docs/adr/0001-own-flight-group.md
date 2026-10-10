# 用自己的合并回源实现（internal/flight）替换 x/sync/singleflight

`GetMany` 要在同一套合并回源里**逐个认领**多个 key，然后把自己领头的那些 key 合成每层一次调用。认领时必须**当场**知道自己是不是领头请求。`golang.org/x/sync/singleflight` 的 `DoChan` 做不到：它在锁内决定谁是领头，但把回源函数放进另一个 goroutine 执行，不把结果告诉调用方。如果为了得到答案而等待，两个互相重叠的批量读会各自等对方手里的 key，从而死锁。所以我们写了 `flight.Group`（内部包 `internal/flight`）：`Claim` 立即返回（在途回源，是否领头），`Finish` 负责发布。`Get`、`GetMany` 和后台刷新共用它，因此它们之间也只回源一次。

`flight.Group[K, T]` 对 key 的类型是泛型的。cachex 用 `flightKey{key, slot}` 做 key，把「每个 key 的回源数」（`WithFetchesPerKey`）收进登记本身，不用拼字符串，认领时也不分配内存。

## 考虑过的方案

- **继续用 x/sync，`GetMany` 自己维护一张登记表**：`Get` 和 `GetMany` 之间就不能合并回源，同一个 key 会被回源两次。
- **整批作为一个 singleflight key**：同一批 key 才能合并，只要有一个 key 不同就要重新回源，也无法和 `Get` 合并。

## 后果

- 有一个和 x/sync 的**有意偏离**：Goexit 时会给等待方发布错误，见 [ADR 0006](0006-goexit-publishes-an-error.md)。
- `Finish` 先移除登记、再发布结果，和 x/sync 的顺序相同。二次检查依赖这个顺序。
- `x/sync` 仍然留在 `go.mod` 里，基准测试拿它的 singleflight 做对照。
