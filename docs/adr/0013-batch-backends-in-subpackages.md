# v2：后端接口必须支持批量，后端拆成子包，字节后端用 Codec

v1 的批量能力是可选接口（`BatchCache`），Client 每次靠类型断言决定走批量还是逐个；一个后端经过 `Transform` 包装，批量能力就悄悄消失。核心包还直接 import 了 gorm、go-redis、ristretto、bigcache，回源逻辑也认识 GORM 事务放在 ctx 里的 key。

v2 的做法：

- **只有一个后端接口 `Backend[T]`**，单 key 和批量方法都要实现，单 key 的 `Get` 用 `(Entry[T], bool, error)` 表示有没有，不用错误表示未命中。只有单 key 能力的存储实现 `SingleBackend[T]`，用 `cachex.Batched` 升级成 `Backend[T]`。
- **数据源仍分两种**：`Source[T]`（必需）和 `BatchSource[T]`（可选）。数据源确实有两种形态，这是真实的分叉，不是包装的产物。
- **后端拆成子包**：`rediscachex`、`gormcachex`、`ottercachex`、`bigcachex`，各自依赖自己的库；核心包只依赖标准库和 `internal/`。测试用的内存后端和时钟在 `cachextest`。
- **核心不再认识 GORM**：核心在回源和回填用的 ctx 上打一个标记（`cachex.IsShared(ctx)`），`gormcachex` 看到这个标记就不用调用方的事务。ADR 0007 的目标不变，依赖方向反过来了：后端认识核心，核心不认识后端。
- **存字节的后端接收 `Codec[T]`**，默认 JSON；条目的时间字段由 `cachex.EncodeEntry`/`DecodeEntry` 统一编码。去掉 `Transform`、`JSONTransform`、`StringJSONTransform`。
- **解码失败算未命中**：后端记一条 WARN 日志并删掉这个条目，读取照常回源。v1 里结构体有不兼容的改动时，解码失败会被当成后端故障，`Get` 一直报错直到条目过期。

## 考虑过的方案

- **批量保持可选，`Transform` 转发批量方法**：能修好 `Transform`，但分派逻辑仍散在核心各处，用户自写的后端也照样会悄悄丢掉批量。
- **子包不带 `x` 后缀**（`rediscache`）：`x` 后缀在 Go 里常表示「扩展某个包」，但和库名 cachex 对应，读者一眼能看出它属于 cachex，所以采用 `rediscachex` 这一组名字。

## 后果

- 自写后端要实现 6 个方法，或者实现 3 个单 key 方法再用 `Batched` 包装。
- 只用内存层的程序不再间接依赖数据库驱动。
- 字节后端的存储格式由 cachex 定义；结构体有不兼容的改动时，仍建议换 `KeyPrefix`，因为 JSON 解码很宽松，字段改名不会解码失败，只会变成零值。
