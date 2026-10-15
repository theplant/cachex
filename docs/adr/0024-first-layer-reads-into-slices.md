# 第一层可选地读进切片，不改 Entry

一个使用方（缓存一个 56 字节的值类型结构体）实测：全部命中内存层时，v2 的 `GetMany` 比 v1 慢，200 个 key 的分配从 14 次变成 214 次，每次查询的后端 CPU 多约 15%，多出来的主要是 GC。原因是 `Backend.GetMany` 返回 `map[string]Entry[T]`：`Entry[T]` 在值之外还带 `NotFound` 和三个 `time.Time`（共 80 字节），值类型稍大，整个条目就超过 128 字节，Go 的 map 不再把它存在桶里，而是每个值单独分配一次。

我们加一个可选接口 `SliceGetter[T]`（`GetInto(ctx, keys, entries, found)`），第一层实现了它，`GetMany` 就把结果读进两个切片，一次调用只分配两次。`ottercachex`、`Encoded`、`cachextest.Map`、`Batched` 都实现了。基准测试 `BenchmarkGetManyHitByValue`：200 个 key 全部命中 otter，耗时少约 22%，分配从 219 次降到 16 次；值是字符串时也快约 20%。

## 考虑过的方案

- **把 `Entry` 的三个时间字段改成 int64**：条目小 48 字节，存进内存层的每个条目也省内存，还不含指针。但 `Entry` 是公开类型，后端和使用方的代码到处用 `time.Time` 字面量，改动面大，换来的类型也更难用；而且值类型再大一些照样超过 128 字节，治标不治本。
- **改 `Backend.GetMany` 的签名**：所有后端都要改，破坏性最大。
- **精简读路径上每个 key 的簿记**：读之前记下每个 key 的分片纪元是二次检查的前提（见 [ADR 0008](0008-on-demand-double-check.md)），不能挪到读之后；剩下能省的只有几次固定的分配，200 个 key 时收益不到 5%，没做。

## 后果

- 新增一个可选接口，第一层实现它才有收益；包装后端（比如测试里的钩子）不转发它时，回到 `GetMany`，行为不变。
- `GetInto` 的语义和 `GetMany` 一致，契约测试 `TestBackend` 检查实现了它的后端读到的和 `Get` 一致。
