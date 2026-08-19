# 稳定性与可运维性

## 当前最低验证线

- 构建：`swift build`
- 单元测试：`swift test`
- 跨平台协议 golden fixtures：`./scripts/test-mcp-protocol-fixtures.sh`（已接入 `scripts/ci.sh`；三端 legacy 与 modern 响应规范化比对，legacy 保持 byte-identical）
- 端到端 smoke：`./scripts/run-tool-smoke-tests.sh`（legacy full + cursor-idle + modern smoke；modern smoke 会对 fixture app 跑一条真实 handle 链，含 stale-reuse 拒绝）
- macOS SkyLight 实机回归：`OPEN_COMPUTER_USE_RUN_SKY_CLICK_LIVE_TEST=1 swift test --filter SkyClickLiveTests`
- Linux runtime：`(cd apps/OpenComputerUseLinux && go test ./...)`、`./scripts/build-open-computer-use-linux.sh --arch arm64`
- 本地诊断：
  - `open-computer-use doctor`
  - `open-computer-use snapshot <app>`

## 已知关键依赖

- macOS 上必须给 `Open Computer Use.app` 授权 `Accessibility` 与 `Screen Recording`；终端本身不应该再是必需授权对象。
- macOS `click_method=sky_click` 额外依赖 SkyLight / ApplicationServices 私有符号 `SLEventPostToPid`、`SLEventSetIntegerValueField`、`CGEventSetWindowLocation`、`SLPSPostEventRecordTo` 和 `GetProcessForPID`。运行时会动态探测并 fail closed，但 macOS 更新、签名方式或目标 app 输入策略变化仍可能让后台投递失效。受控实机回归除 DOM、前台 PID、鼠标和 z-order 外，还必须验证前台 AppKit active、key window、first responder 以及 resign/key-loss 计数。
- smoke suite 依赖本地 GUI session，不能把它当成无头环境命令。
- 普通 app 的 `get_app_state` 结果依赖 AX tree 和窗口截图，复杂 app 上输出会有差异；Electron/WebView app 的 AX tree 通常很深，当前会压缩空 wrapper 并放宽遍历深度，以优先保留可操作文本、按钮和输入框。
- snapshot 状态不再只靠进程内存维持：modern `2026-07-28` era 的 element_index / focus / 坐标状态由显式的 bounded `SnapshotHandleStore` 承载，action 通过 `snapshot_ref` 绑定到具体 PID/window/generation，绝不在 dispatch 前重新采集旧 index；handle 有 120 秒绝对 TTL、单 live generation、16 target / 64 tombstone 上界，过期或跨代复用会在任何输入前返回可恢复错误。legacy `2025-03-26` era 在兼容窗口内仍保留旧的进程内隐式 cache。macOS 上 store 属于长期存活的 app agent，socket 连接关闭后 handle 仍可解析；Linux / Windows 上进程重启会返回 unknown/expired handle 错误而不是静默换用新状态。
- Linux runtime 依赖已登录桌面用户 session；缺少 `XDG_RUNTIME_DIR`、`DBUS_SESSION_BUS_ADDRESS` 或 display 环境时，会尝试从 `/run/user/<uid>` 和常见桌面进程自动发现当前用户的 session env。纯 SSH tty 如果找不到桌面 session 仍不能直接访问 AT-SPI GUI tree。
- GNOME Wayland 截图可能被 compositor 限制，当前 Linux bridge 会把黑图视为无效截图并省略 image block。

## 当前故障排查顺序

1. 先跑 `open-computer-use doctor`，确认权限状态；如果缺权限，命令会通过 `.app` app agent 拉起权限 onboarding 窗口，已全部授权则只打印状态并退出。
2. 用 `open-computer-use list-apps` 确认目标 app 是否被发现。
3. 用 `open-computer-use snapshot <app>` 看是 transport 问题还是 snapshot / action 问题。
4. 如果只有 `sky_click` 失败，先重新执行 `get_app_state`，确认窗口仍为 on-screen、未隐藏/最小化且没有切换 Space；错误里出现 `missing SkyLight symbols` 时不要改用隐式 fallback，应按当前 macOS 版本重新验证私有 SPI。被遮挡的 Chromium 页面仍无效果时，再用受控页面区分 renderer 策略变化与坐标/window-local 映射问题。
5. 如果只想验证仓库基线，直接跑 fixture + smoke，不要先在复杂第三方 app 上排查。
6. 排查 Linux runtime 时，先确认目标命令是否由桌面用户运行，再用 `open-computer-use call list_apps` 和 `open-computer-use snapshot <app>` 区分 session/env 问题与 AT-SPI tree/action 问题。如果是 Codex MCP，重新执行 `open-computer-use install-codex-mcp` 后重启 Codex，确认配置仍是 `open-computer-use mcp`。

## 后续补强方向

- 增加结构化日志和失败原因分类。
- 继续补充 screenshot capture / AX traversal 的失败上下文和普通 app 回归样本。
- 增加普通 app 回归样本，而不是只覆盖 fixture。

CI/CD 流程结构和 release 自动化的默认方案，统一写在 `docs/CICD.md`。
