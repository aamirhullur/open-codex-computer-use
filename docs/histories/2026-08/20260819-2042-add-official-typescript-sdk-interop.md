## [2026-08-19 20:42] | Task: Add official TypeScript SDK interoperability

### 🤖 Execution Context
* **Agent ID**: `Codex`
* **Base Model**: `GPT-5.6`
* **Runtime**: `T3 Code through the Codex harness`

### 📥 User Query
> Implement only M6.1: validate the existing dual-era stdio MCP server with the official TypeScript v2 client in auto-modern, pinned-modern, and explicit-legacy modes, including the snapshot handle lifecycle.

### 🛠 Changes Overview
**Scope:** Test-only official MCP SDK dependency, macOS fixture isolation seam, and stdio interoperability harness.

**Key Actions:**
- **Official SDK harness**: Added a pinned `@modelcontextprotocol/client@2.0.0` development dependency and an ESM test that drives the built `OpenComputerUse mcp` server through `StdioClientTransport`.
- **Dual-era acceptance**: Asserted official-client negotiation, exact nine-tool order and era-specific schemas, a modern two-action snapshot successor chain followed by zero-effect stale-handle rejection, and legacy ref-less state/action behavior.
- **Safe fixture lifecycle**: Added an explicit fixture state-root override that scopes both state files and distributed commands, so default and isolated fixtures cannot consume each other's actions. The harness passes only an allowlisted child environment, tracks and closes every active SDK transport before terminating its owned fixture, bounds both graceful and forced shutdown waits, and removes only its unique temporary directory through one shared cleanup operation.

### 🧠 Design Intent (Why)
The raw protocol fixtures already prove the wire contract, but they do not prove an independently maintained official client can negotiate and decode it. This test adds that evidence without changing the production Swift or Go protocol adapters, adding a production HTTP transport, or turning SDK packages into runtime dependencies.

### 📁 Files Modified
- `.gitignore`
- `package.json`
- `package-lock.json`
- `scripts/test-mcp-sdk-interop.mjs`
- `apps/OpenComputerUseFixture/Sources/OpenComputerUseFixture/main.swift`
- `packages/OpenComputerUseKit/Sources/OpenComputerUseKit/FixtureBridge.swift`
- `packages/OpenComputerUseKit/Tests/OpenComputerUseKitTests/FixtureBridgeTests.swift`
