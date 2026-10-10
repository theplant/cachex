# 批量读的返回形态

日期：2026-10-09。对应决策：[ADR 0004](../adr/0004-batch-result-shape.md)。

## 问题

`GetMany` 该返回哪种形态？从性能角度看，哪种最快？

- A. `map[string]T`：不存在的 key 不放进 map（现在的做法）
- B. `map[string]Result[T]`：每个 key 都有一项，不存在用 `Err = ErrKeyNotFound` 表示
- C. `[]Result[T]`：按请求 key 的顺序一一对应

## 方法

构造结果的开销，一半 key 命中、一半不存在（缓存中常见的情况）。不存在用一个共享的错误实例，不为每个未命中单独分配。

## 结果（Apple M 系列，Go 1.27）

| 形态 | 100 个 key | 10000 个 key | 内存（10000 个 key） |
|---|---|---|---|
| A. `map[string]T` | 633 ns | 66 µs | 328 KB |
| B. `map[string]Result[T]` | 1271 ns | 171 µs | 918 KB |
| C. `[]Result[T]` | 279 ns | 38 µs | 328 KB |

- C 最快，但只比 A 快约 1.7 倍。B 最慢，耗时和内存都接近 A 的 3 倍：它要给每个未命中也放一项，而且每项多一个 error 接口（16 字节）。
- 放到整次 `GetMany` 里看，差别很小：10000 个 key 时，A 和 C 相差不到 30µs，而一次 Redis 往返就要几百微秒，还有 10000 个值的解码。
- 调用方拿到 C 之后，大多还是要按 key 查值，等于把建 map 的成本转移给了调用方。

所以性能不是决定因素。决定因素是：C 有「结果错位、拿到别的 key 的值」的风险，B 要求实现方为每个缺席的 key 补一项。在两者都安全的前提下，A 是最快的。

如果 `ErrKeyNotFound` 每次都用 `pkg/errors` 包一层，会额外抓取调用栈。B 方案下每个未命中都要付这个代价，所以如果真要采用 B，必须使用共享实例。

## 复跑

```sh
go test -tags bench -bench . ./tools/bench/2026-10-batch-result-shape/
```
