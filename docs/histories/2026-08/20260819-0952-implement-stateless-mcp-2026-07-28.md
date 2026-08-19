## [2026-08-19 09:52] | Task: Implement stateless MCP 2026-07-28

### 🤖 Execution Context
* **Agent ID**: `Claude Code`
* **Base Model**: `Claude Opus 4.8`
* **Runtime**: `Claude Agent SDK`

### 📥 User Query
> 实现 stateless MCP `2026-07-28` 迁移：在 macOS、Linux、Windows 三端交付 dual-era stdio MCP server，符合 `2026-07-28`，同时保留 `2025-03-26` 客户端；把 modern action tool 原本对进程内存 snapshot cache 的隐式依赖，换成由 `get_app_state` 与后继 action 结果显式返回的 `snapshot_ref`。（M0-M5 分阶段推进，最后补文档、history 与执行计划对账。）

### 🛠 Changes Overview
**Scope:** MCP 协议层与 tool service（macOS Swift `OpenComputerUseKit`、Linux/Windows Go runtime、smoke suite、CI 脚本、文档）

**Key Actions:**
- **Dual-era 协议适配**: 三端 stdio server 按连接首条请求做一次 era 分类；modern `2026-07-28` era 带 per-request `_meta`、`server/discover`、集中补 `resultType: "complete"` + serverInfo、`server/discover` 与 `tools/list` 的 `ttlMs: 300000` / `cacheScope: "public"`、移除 modern `ping`；legacy `2025-03-26` era 保持 byte-identical，跨 era 切换双向拒绝。
- **显式 snapshot 状态**: 新增 bounded `SnapshotHandleStore`（`ocu_snapshot_v1_` + 24 随机字节 handle、120 秒绝对 TTL、单 live generation per target、16 live target / 64 tombstone、先过期后 least-recently-created 驱逐、redacted 日志、constant-time resolve、退休 payload zeroing）；macOS 上归长期存活的 app agent 持有，Linux/Windows 暂归 MCP 进程。
- **事务化 modern action**: 7 个 action tool 在任何副作用前校验 `snapshot_ref`，per-target CAS `live`->`in_flight`，复核 identity/PID/window 与元素/坐标，仅从存储 snapshot 发一次 native dispatch，返回 generation n+1 后继 handle；错误码含 `snapshot_ref_missing`/`_malformed`/`_unknown`/`_expired`/`_stale`/`_in_use`、`snapshot_target_changed`、`snapshot_action_outcome_uncertain`，三端消息字符串一致。
- **CLI 与验证**: `call --calls` batch 支持显式 `snapshot_ref` 自动 thread、`OPEN_COMPUTER_USE_STRICT_SNAPSHOTS=1` opt-in strict gate；新增三端共享协议 golden fixtures（接入 `scripts/ci.sh`）、`test-mcp-conformance.sh` 诚实降级包装，smoke suite 增加真实 handle 链的 modern smoke。
- **文档对账**: 同步 ARCHITECTURE / RELIABILITY / SECURITY / QUALITY_SCORE / design index / release notes / tech-debt-tracker / README(中英)/ troubleshooting，并对齐执行计划状态。

### 🧠 Design Intent (Why)
旧实现里 action 的正确性依赖“两次请求恰好共享同一进程”的隐式 snapshot cache，index `42` 在新采集的树里可能指向不同元素，属于 stateless 调用下的不安全 fallback。MCP `2026-07-28` 明确允许 server mint handle、client 显式带回，从而把这条应用状态依赖变得可见、有界、可恢复；dual-era 则保证既有 `2025-03-26` host 不被破坏。三端共享 golden fixtures 用来防止协议行为在三套实现间漂移。记录在案的 conformance gap：官方 `@modelcontextprotocol/conformance`（0.1.16）尚不认识 `2026-07-28` 且仅支持 HTTP server，本 server 只有 stdio，因此设计验收标准 9 目前无法用官方工具满足，执行计划就此保持开放。

### 📁 Files Modified
- `packages/OpenComputerUseKit/Sources/OpenComputerUseKit/ComputerUseToolDispatcher.swift`
- `packages/go-mcp/state.go`
- `apps/OpenComputerUseLinux/main.go`
- `apps/OpenComputerUseWindows/main.go`
- `apps/OpenComputerUseSmokeSuite/Sources/OpenComputerUseSmokeSuite/main.swift`
- `scripts/ci.sh`, `scripts/run-tool-smoke-tests.sh`, `scripts/test-mcp-conformance.sh`
- `tests/mcp-protocol-fixtures/`
- `docs/ARCHITECTURE.md`, `docs/RELIABILITY.md`, `docs/SECURITY.md`, `docs/QUALITY_SCORE.md`
- `docs/design-docs/index.md`, `docs/releases/feature-release-notes.md`, `docs/exec-plans/tech-debt-tracker.md`
- `docs/exec-plans/active/20260813-stateless-mcp-2026-07-28.md`
- `README.md`, `README.zh-CN.md`, `skills/open-computer-use/references/troubleshooting.md`
