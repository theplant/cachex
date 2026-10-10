# 上游 Goexit 时给所有等待方发布错误（有意偏离 x/sync）

上游调用 `runtime.Goexit`（现实中基本只来自测试里在上游函数中调用 `t.FailNow`）时，cachex 给这次回源的所有等待方发布错误 `upstream fetch exited without returning (runtime.Goexit)`，和 panic 时的处理方式一致。这**有意偏离**了 main 上 `x/sync/singleflight` 的行为。

## x/sync 是怎么做的，为什么不照搬

x/sync 的设计原则是「同一个 key 的所有调用方，得到和自己亲自调用一样的结局」：用 `Do` 时，回源 panic，等待方也 panic；回源 Goexit，等待方也 Goexit。但 `DoChan` 的回源跑在另一个 goroutine 里，没法把结局复制给等待方：panic 时它故意让整个进程崩溃；Goexit 时它**什么都不发送**，等待方要一直等到自己的 ctx 结束，没有截止时间就永远挂着。2022 年有人报过这个泄漏（[golang/go#52557](https://github.com/golang/go/issues/52557)），最终没修就关了，所以这不是刻意设计。main 上的 cachex 用的就是 `DoChan`，继承了这个泄漏。

main 对 panic 的处理是在回源函数里 recover、转成错误发给所有等待方，从来没有走到 x/sync 的崩溃路径。把 Goexit 也转成错误，和 main 对 panic 的处理正好一致。

## 后果

- 等待方不会再因为 Goexit 永远挂着。这类测试会很快失败，而不是挂到超时。
- 每个在途回源恰好发布一次结果。单 key 和批量共用同一个小结构 `claimed` 来保证这一点：批量回源中途 panic 或 Goexit 时，只有还没拿到结果的 key 收到错误。
- 后台批量刷新原来要给等待加一个超时，防止某个回源永远不发布结果、导致这些 key 永远不再刷新。有了这个保证，那个超时就删掉了。
- `get_compat_test.go` 里对应的用例按新行为修改了，测试里注明了这是和 main 的有意差异。
