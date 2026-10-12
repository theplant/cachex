# 核心包拒绝没法和租约同 slot 的 key

租约（见 [consistency.md](../design/consistency.md#租约lease)）在 Redis 上用一段 Lua 同时操作数据 key 和租约 key。Redis Cluster 要求一段脚本碰的 key 在同一个 slot，所以租约 key 要从数据 key 推出来：数据 key 带 hash tag（第一个 `{` 和它后面第一个 `}` 之间不为空）时在前面加前缀（`cachex-lease:t:`），不带时把整个 key 包进 `{}`（`cachex-lease:{key}`）。只有一种 key 推不出来：含 `}`、却没有 hash tag，比如 `a}b`。

我们让**核心包**拒绝这种 key，不管有哪些层、开没开租约：`Get`、`GetMany`、`Set`、`SetMany` 返回 `ErrUnsupportedKey`，`Del`、`DelMany` 什么也不做（它从来没被存过）。`Get` 只在未命中时检查，因为没有哪一层会存这种 key，命中路径不多花一次扫描。

## 考虑过的方案

- **只在 rediscachex 里拒绝**：核心包按约定不认识任何后端（[ADR 0013](0013-batch-backends-in-subpackages.md)），这样最干净。但一开始只用内存层、后来加上 Redis 的用户，会在加层那天才发现有的 key 不能用了。我们宁可让这条限制从第一天就生效。
- **只在打开租约、或者只在 Cluster 上拒绝**：同样的问题，换成了打开租约那天、迁到 Cluster 那天。
- **打开租约时统一给数据 key 加 hash tag**：所有 key 都能用，但存储格式变了，打开租约就等于换一份数据。
- **这种 key 不用租约，无条件回填**：在最少见的 key 上悄悄失去保护，排查时很难想到。

## 后果

- 核心包里有一条来自 Redis 的规则，写在 `ErrUnsupportedKey` 的注释里。别的后端不需要它，但用户只需要记一套 key 规则。
- 受影响的 key 很少：带 hash tag 的（`{user:1}:profile`）、只含 `{` 的都能用。
- 这是破坏性变更，写进了 MIGRATION。
