# 批量写尽力而为

后端的 `SetMany`/`DelMany` 对每个 key 都尝试写，失败的 key 列在原样返回的 `*BatchError` 里，其余照常写入；返回其他 error 表示不知道哪些写成功了。所有内置后端都照此实现：Redis 逐条命令判断错误；GORM 每段一条语句、独立提交，某段失败就继续写后面的段；`Batched` 逐个 key 进行。理由是：回填时多缓存一个 key 就少一次回源；写入时 `Cache` 能按 key 处理失败（只删失败的 key，见 [ADR 0002](0002-upstream-first-writes.md)），而整批回滚只会让本来能写进去的 key 也一起失败。

## 考虑过的方案

- **全有或全无**：需要事务，Redis 则要改用 MULTI/EXEC；某一段失败会连累整批，对缓存没有好处。
- **各后端各行其是，只在文档里写清楚**：`Cache` 无法写出通用的错误处理。

## 后果

- 错误规则和 `GetMany` 一致：原样返回的 `*BatchError` 表示部分失败，其他 error 表示整批失败。
- `Cache.SetMany`/`DelMany` 把失败的 key 列在 `*BatchError` 里，其余 key 照常写完。
- GORM 的一次大批量写入不是原子的。对缓存来说这没有问题。
- 在调用方自己的事务里（`gormcachex.WithTx`），某条语句失败后，PostgreSQL 会让整个事务失效，后面的段也会失败。它们会被如实报告在 `BatchError` 里。
