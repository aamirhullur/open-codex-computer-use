# 技术债追踪

这里记录那些暂时不阻塞当前任务、但已经值得留档的技术债。

| 日期 | 区域 | 债务描述 | 为什么会存在 | 计划中的后续动作 |
| --- | --- | --- | --- | --- |
| 2026-04-17 | 普通 app AX snapshot | Finder 路径已经能拿到前台窗口子树并输出 window-relative frame，但当前还缺更多真实 app 回归样本，无法证明这套 rooting / traversal 对复杂 app 都稳定。 | 这一轮先把 Finder 这类真实 app 的坐标换算和窗口子树收敛好，再把 deterministic 回归继续留给 fixture。 | 增加 Safari / System Settings / Activity Monitor 等真实 app 样本验证，并继续收敛 `kAXMainWindowAttribute`、focused element parent chain 和多窗口回退策略。 |
| 2026-08-19 | snapshot handle 测试 | modern snapshot 期望校验用了一个 ellipsis-name 启发式：当 `expected_name` 以 `...` 结尾时跳过名称比对。这依赖字符串形态推断截断，语义不够精确。 | snapshot 文本默认截断到 500 字符并追加 `...`，测试为了不把截断名误判成 target changed 而临时按后缀跳过。 | 用一个显式的 runtime truncation flag 替换后缀启发式，让校验基于“该字段是否被截断”这一确定信号而不是字符串结尾。 |
| 2026-08-19 | MCP conformance | 官方 `@modelcontextprotocol/conformance`（0.1.16）不认识 spec version `2026-07-28`（只接受 `2025-03-26`、`2025-06-18`、`2025-11-25`、`draft`、`extension`），且 server 模式仅支持 HTTP，而本 server 只有 stdio；设计验收标准 9 目前无法用官方工具满足。 | 上游 conformance 工具尚未跟进 `2026-07-28`，也未提供 stdio server 校验路径；modern 协议验证暂时依赖仓库内 golden fixtures 与 suite，raw-stdio handshake 已端到端验证正确。 | 等上游 conformance 支持 `2026-07-28` 且提供 stdio server 模式后，接入 `./scripts/test-mcp-conformance.sh --spec-version 2026-07-28` 并勾掉设计验收标准 9。 |
| 2026-08-19 | MCP SDK 互操作 | 官方 SDK 目前只能通过 legacy `2025-03-26` era 与本 server 互通（暂无 TS SDK v2，最新 `@modelcontextprotocol/sdk` 1.30.0，最高协议 `2025-11-25`）；modern era 需要能讲 per-request `_meta` + `server/discover` 的客户端。 | 设计 open question 5：实现期先用仓库自有的小协议适配层，把官方 SDK 采用推迟到 conformance 通过之后再评估。 | 一旦出现能讲 `2026-07-28` 的官方 Swift / Go / TS SDK，重新评估是否用 SDK 替换仓库自有适配层，并把决策记到执行计划。 |
