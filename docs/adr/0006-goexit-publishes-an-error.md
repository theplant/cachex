# 数据源 Goexit 时给所有等待方发布错误（有意偏离 x/sync）

数据源调用 `runtime.Goexit`（现实中基本只来自测试里在数据源函数中调用 `t.FailNow`）时，cachex 给这次回源的所有等待方发布错误 `cachex: fetch exited without returning (runtime.Goexit)`，和 panic 时的处理方式一致。这**有意偏离**了 `x/sync/singleflight` 的行为。

## x/sync 是怎么做的，为什么不照搬

x/sync 的设计原则是「同一个 key 的所有调用方，得到和自己亲自调用一样的结局」：用 `Do` 时，回源 panic，等待方也 panic；回源 Goexit，等待方也 Goexit。但 `DoChan` 的回源跑在另一个 goroutine 里，没法把结局复制给等待方：panic 时它故意让整个进程崩溃；Goexit 时它**什么都不发送**，等待方要一直等到自己的 ctx 结束，没有截止时间就永远挂着。2022 年有人报过这个泄漏（[golang/go#52557](https://github.com/golang/go/issues/52557)），最终没修就关了，所以这不是刻意设计。v1 的前身用的就是 `DoChan`，继承了这个泄漏。

cachex 对 panic 的处理一直是 recover、转成错误发给所有等待方，从来不走 x/sync 的崩溃路径。把 Goexit 也转成错误，和对 panic 的处理正好一致。

## 后果

- 等待方不会再因为 Goexit 永远挂着。这类测试会很快失败，而不是挂到超时。
- 每个在途回源恰好发布一次结果。单 key 和批量共用同一个小结构 `claimed` 来保证这一点：批量回源中途 panic 或 Goexit 时，只有还没拿到结果的 key 收到错误。
- 后台刷新不需要给等待另加超时：不存在永远不发布结果的回源。
