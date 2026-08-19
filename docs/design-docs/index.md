# 设计文档索引

用这个目录集中管理架构设计和产品设计文档。

建议约定：

- 一个主题一份文档。
- 每份文档写清当前状态和简短摘要。
- 关联引入它的 execution plan 或 spec。

## 初始文档

- `core-beliefs.md`

## Implemented designs (with recorded gaps)

- `stateless-mcp-2026-07-28.md`: dual-era MCP `2026-07-28` migration, explicit snapshot handles, compatibility rollout, and verification criteria. Implemented on `agent/stateless-mcp-implementation-spec` (M0-M5), with modern-protocol validation resting on in-repo golden fixtures and suites. Recorded gap: the official conformance suite does not yet know spec version `2026-07-28` and is HTTP-only, so design acceptance criterion 9 cannot currently be satisfied by official tooling; the implementation plan `docs/exec-plans/active/20260813-stateless-mcp-2026-07-28.md` is held open pending upstream support.
