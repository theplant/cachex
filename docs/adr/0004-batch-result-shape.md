# 批量读返回 (map, error)，缺席即不存在

`Cache.GetMany`、`BatchSource.GetMany` 和后端的 `GetMany` 都返回 `(map, error)`：在 map 里表示找到，不在 map 里表示不存在，失败的 key 列在原样返回的 `*BatchError` 里；其他 error 表示整批失败。不存在**不算错误**，也不放进 `BatchError`。理由是：缓存的批量读几乎每次都有未命中，如果把不存在算作错误，`err` 就几乎总是非 nil，调用方每次都得过滤；而实现批量数据源的人最自然的产出就是「查到的那些行」，比如 SQL 的 `WHERE id IN` 只返回存在的行。

## 考虑过的方案

| 方案 | 不选的原因 |
|---|---|
| `[]Result[T]`，按请求 key 的顺序一一对应 | 实现方必须保证第 i 个结果对应第 i 个 key。SQL 查回来的行是无序的，一旦排错，调用方会拿到**别的 key 的值**，而且没有任何报错。map 以 key 为索引，从结构上就排除了这种错误 |
| `map[string]Result[T]`，不存在用 `Err = ErrNotFound` 表示 | 实现方必须对照请求逐个补上不存在的 key；每个未命中都要占一项。实测耗时和内存都约为方案 A 的 3 倍，见 [research/2026-10-batch-result-shape.md](../research/2026-10-batch-result-shape.md) |
| 一个 `BatchResult{Values, Errors}` 结构体 | 本质和现在一样，只是丢掉了 error 接口，不能写 `if err != nil` |

## 后果

- `Get` 和 `GetMany` 用各自的方式表达同一套三种结果（找到、不存在、失败）：`Get` 用哨兵错误 `ErrNotFound`，`GetMany` 用缺席。
- 值可以是 nil，判断 key 在不在必须用 `v, ok := m[k]`。
- 只有原样返回的 `*BatchError` 才算部分失败；包过一层的算整批失败。整批失败的错误里包着 `ErrNotFound` 是实现方的错误用法，`errors.Is` 会认出它。
- 命名：方法用 `GetMany`（这次操作多个 key），类型用 `Batch*`（这个东西支持批量）。没有用 `BatchGet` 的一个原因是，Google API 规范（AIP-231）规定 `BatchGet` 必须是原子的，而这里允许部分成功。
