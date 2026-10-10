# 字节后端统一经 cachex.Encoded

[ADR 0013](0013-batch-backends-in-subpackages.md) 定下「字节后端解码失败算未命中」，但这条规则只写在 `DecodeEntry` 的注释里，由每个后端自己照做。`bigcachex`、`rediscachex`、`gormcachex` 因此各有一份几乎一样的代码：用 `EncodeEntry`/`DecodeEntry` 编解码条目、解码失败记 WARN 并删掉、已经腐烂的条目改为删除，外加各自的剩余时间算术。三份里任何一份写偏（比如把解码失败当成后端故障），值的类型一改，读取就一直报错，直到后端让条目过期，而契约测试要给每个后端构造坏数据才测得到。

我们把这些挪进核心包：

- **`ByteBackend`**：只按 key 存取字节，`Set` 多带一个过期时间，批量方法用 `ByteEntry{Data, ExpiresAt}`；未命中和批量错误的约定和 `Backend` 相同。
- **`Encoded[T](b, codec, logger) Backend[T]`**：负责条目的编码（`codec` 为 nil 时用 `DefaultCodec`）；数据解不出来时记一条 WARN `cachex: dropped an entry that does not decode`、删掉这个 key、报告没有；已经过了 `ExpiresAt` 的条目不写，改为删除。
- **「已经过了」按真实时钟（`time.Now`）判断**，不按 `Cache` 的 `WithNow`：后端的过期（Redis 的 TTL、otter 的到期淘汰、BigCache 的 `LifeWindow`）都按真实时钟进行，`Encoded` 也拿不到 `Cache` 的时钟。
- 三个子包的字节后端改为私有，`New` 返回 `Encoded` 包好的后端。`cachextest.TestByteBackend` 是字节后端的契约测试。

## 考虑过的方案

- **保持各后端各写一份，只补契约测试**：规则仍然靠照抄，自写的字节后端也得再抄一遍；坏数据的测试要每个后端各构造一次。
- **按 `Cache` 的时钟判断条目是否已经腐烂**：要把时钟传进每个后端；而且后端的过期本来就按真实时钟，按另一个时钟判断，只会让两者对不上。

## 后果

- `rediscachex.New`、`bigcachex.New` 返回 `cachex.Backend[T]`，原来的 `rediscachex.Backend`、`bigcachex.Backend` 类型没有了；`gormcachex.New` 仍返回 `*gormcachex.Backend[T]`，它嵌入 `cachex.Backend[T]`，另有 `Migrate`。
- 自写的字节存储实现 `ByteBackend`，再用 `Encoded` 包装；存值本身、只有单 key 能力的存储仍然实现 `SingleBackend` 再用 `Batched` 包装。
- `WithNow` 设的时钟离真实时间很远时（比如从 1970 年开始的 `cachextest.Clock`），按它算出的 `ExpiresAt` 在真实时钟看来早已过去，字节后端和 `ottercachex` 会当场丢掉条目，于是每次都未命中。这类测试用 `cachextest.Map`，或者让测试时钟从 `time.Now()` 开始。
- 多一层间接调用：`bigcachex` 第一层命中约慢 8ns（94ns → 102ns）；写入的基准变化在 5% 以内；第二层命中反而快了约 38%（`Get/hit/l2` 3.99µs → 2.48µs）。
- `Encoded.SetMany` 遇到同一批里既有未腐烂、又有已经腐烂的条目时，要发一次 `SetMany` 和一次 `DelMany`。gormcachex 的语句各自提交、不进调用方的事务（[ADR 0017](0017-gorm-never-joins-a-caller-transaction.md)），两遍加锁不会互相死锁。
