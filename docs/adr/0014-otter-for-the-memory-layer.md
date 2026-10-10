# v2：内存层用 otter 替换 ristretto，BigCache 改为按字节存储

v1 的 `RistrettoCache` 有几处问题：

- `DefaultRistrettoCacheConfig` 的 `MaxCost: 1<<30` 注释写的是 1GB，但每次 `Set` 传的成本是 1，加上内部开销，容量其实是一千多万个条目，值的大小完全不计入，缓存大对象时内存没有上限。
- `NumCounters: 1e7` 每个实例固定占约 30MB；v1 再配一个 ristretto 做不存在缓存就是两份。
- 每次 `Set` 都调用 `Wait()` 才能保证「写完能读到」，所有写入因此串行。
- 写入缓冲区满或被准入策略拒绝时，`Set` 会被静默丢弃。
- 在偏重「最近访问」的负载下，命中率明显低于 W-TinyLFU。

v2 的内存层 `ottercachex` 基于 otter v2：写入同步可见；`MaximumSize` 按条目数或 `MaximumWeight` 按字节计算容量，必须由用户给出；每个条目按条目的 `ExpiresAt` 单独过期。

BigCache 保留为 `bigcachex`：值按 `Codec` 编码成字节存储。GC 要扫描的是堆上存活的指针，内存层有数百万条带指针的值时，GC 的开销会很明显，这时把值存成字节可以换来更低的 GC 开销，代价是每次读取都要解码。

## 考虑过的方案

- **继续用 ristretto，只修默认配置**：能修好容量，但 `Wait()` 的串行和静默丢弃是它的设计，改不了。
- **去掉 BigCache**：条目少时它比 otter 慢（每次读取都要解码），但条目多到 GC 明显吃 CPU 时，它是唯一的选择，而适配它只要几十行。

## 后果

- otter 是 Apache-2.0。只有 import 了 `ottercachex` 或 `bigcachex` 的程序会链接进这些代码，向他人分发二进制时要附上它们的 LICENSE。
- `bigcachex` 里过期的条目由 BigCache 自己的 `LifeWindow` 和容量淘汰清理，Cache 读取时按 `ExpiresAt` 判断是否可用。
- 内存后端返回的值是同一份：`T` 是指针时，调用方修改返回的值会改坏缓存。`bigcachex` 每次解码出新值，没有这个问题。
