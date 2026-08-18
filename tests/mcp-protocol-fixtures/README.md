# MCP protocol golden fixtures

Cross-platform golden fixtures for the stdio MCP server. The same fixtures are
driven against three handlers:

- Swift (macOS): `StdioMCPServer.handle(line:)`, via
  `packages/OpenComputerUseKit/Tests/OpenComputerUseKitTests/MCPProtocolFixtureTests.swift`.
- Go (Linux): `handleMCPRequest`, via
  `apps/OpenComputerUseLinux/mcp_fixtures_test.go`.
- Go (Windows): `handleMCPRequest`, via
  `apps/OpenComputerUseWindows/mcp_fixtures_test.go`.

They freeze the current legacy `2025-03-26` behavior before any behavior change
(slice M0) and pin the modern `2026-07-28` target shapes as documented expected
failures until M1/M2 implement them.

Run everything with `./scripts/test-mcp-protocol-fixtures.sh`.

## Layout

```
tests/mcp-protocol-fixtures/
  legacy/                shared legacy cases (byte-identical on all platforms)
  legacy/macos/          legacy cases specific to the Swift handler
  legacy/linux/          legacy cases specific to the Linux handler
  legacy/windows/        legacy cases specific to the Windows handler
  modern/                modern 2026-07-28 target cases (currently expected-fail)
  modern/EXPECTED_FAILURES.<platform>.json
```

The interface contract asks for `legacy/` and `modern/` subdirs. The per-platform
`legacy/<platform>/` subdirs are a deliberate extension: the legacy `initialize`
response embeds platform-specific `instructions`, and legacy `tools/list` embeds
platform-specific tool `description` text, so those two cases cannot be a single
shared golden file. Every runner loads `legacy/` plus its own
`legacy/<platform>/` directory. Cases whose normalized bytes are identical on all
three platforms (ping, notifications, errors, deterministic tool errors) live in
the flat `legacy/` directory and prove cross-platform protocol identity.

## Fixture format

One JSON file per case. ASCII only.

```json
{
  "name": "unique-case-name",
  "description": "what this case freezes",
  "steps": [
    { "request": { "...json-rpc object..." }, "expect": { "...response..." } }
  ]
}
```

- `request`: a JSON-RPC object. The runner serializes it to a single line and
  feeds it to the handler.
- `request_raw`: an alternative to `request`, a verbatim string line. Used for
  the parse-error case, where the input is not a JSON-RPC object. Swift feeds it
  straight to `handle(line:)`; Go mirrors `runMCP`'s decode step (attempt to
  decode into an object, emit `-32700` on failure).
- `expect`: the expected response object, or `null` for notifications and other
  inputs that produce no response line. Store it already in normalized form
  (for example `serverInfo.version` is written as `<VERSION>`); normalization is
  idempotent, so the runner normalizes both sides before comparing.
- `steps` is a list so multi-step sequences (for example the modern
  probe-then-call sequence) run against one server/service instance. Each case
  gets a fresh server and service; state does not leak between cases.

## Normalization rules

Both runners implement identical normalization, then compare canonical JSON
(object keys sorted):

1. Recursively sort object keys (done at compare time by the JSON encoder).
2. Replace a `serverInfo` object's `version` with `<VERSION>`. In practice any
   object carrying both a `name` and a `version` key has its `version` frozen;
   this covers legacy `serverInfo` and the modern
   `io.modelcontextprotocol/serverInfo` block.
3. Replace any string beginning with `ocu_snapshot_v1_` with `<SNAPSHOT_REF>`.
4. Replace any string that is a full ISO-8601 timestamp with `<TIMESTAMP>`.

Volatile per-run fields (pids, screenshots, element trees) never appear in these
protocol fixtures because the cases avoid tools that capture real UI.

## Determinism choices (legacy)

- `list_apps` success is not used. On the Go handlers driven on this build it
  needs the platform runtime (unavailable), and on Swift it enumerates the real
  running apps (non-deterministic). Per the interface contract's normalization
  note, the deterministic tools/call case `tools-call-missing-app`
  (`get_app_state` with no `app`) stands in for it: it exercises the tools/call
  result envelope but fails argument validation before capturing any UI, so it is
  stable across platforms and process runs.
- `tools-call-unknown-tool` also fails inside the dispatcher before any UI and is
  byte-identical across platforms.
- Parse error: the shared case feeds `[]` (well-formed JSON that is not a
  JSON-RPC object), which yields an identical `-32700` on all three handlers.
  Note a real behavioral divergence frozen implicitly here: the Swift handler
  returns `-32700` only for well-formed-JSON-that-is-not-an-object; for truly
  malformed (unparseable) JSON it currently falls into a generic catch and
  returns a tool-style error result whose message is environment-dependent
  (an `NSCocoaErrorDomain` string). The Go handlers return `-32700` for both.
  The malformed-input path is intentionally not frozen as a golden case because
  its message is not stable; a later slice should reconcile this divergence.

## Modern fixtures and expected failures

Modern cases in `modern/` encode the `2026-07-28` target wire shapes from the
design doc (`docs/design-docs/stateless-mcp-2026-07-28.md`, sections "Normative
protocol behavior", "server/discover", and "Tool-list caching"). The current
handlers do not implement the modern era, so every modern case currently
mismatches its target shape.

Per-platform manifests `modern/EXPECTED_FAILURES.<platform>.json` (macos, linux, windows) map each modern case `name` to the reason it
currently fails. A runner:

- treats a listed case as passing while it mismatches (proving the case executes
  and the feature is not yet implemented);
- fails if a listed case unexpectedly matches (a signal the feature landed and
  the entry should be removed);
- enforces any modern case that is not listed, exactly like a legacy case.

Modern error MESSAGE strings in these fixtures (for example "Unsupported
protocol version") are this repository's contract choice: the design pins only
the JSON-RPC error codes and the `data` payloads (such as `supported` and
`requested`), not the human-readable messages. The messages are frozen here so
M1 has a concrete target, but they may be adjusted when M1 lands as long as the
codes and data payloads match the design.

This keeps the suite green at M0 and flips it to enforcing as M1 and M2 land.
Where a modern case includes platform-specific text (instructions, tool
descriptions), the target uses a placeholder (`<INSTRUCTIONS>`,
`<MODERN_TOOL_LIST>`); those placeholders are refined into per-platform frozen
values when the corresponding case leaves that platform's EXPECTED_FAILURES manifest.

## Regenerating

Legacy expectations are captured from the real handlers, not hand-written. Drive
each handler over stdio with the app-agent proxy disabled and the visual cursor
off, for example:

```bash
env OPEN_COMPUTER_USE_DISABLE_APP_AGENT_PROXY=1 OPEN_COMPUTER_USE_VISUAL_CURSOR=0 \
  .build/debug/OpenComputerUse mcp < requests.ndjson
(cd apps/OpenComputerUseLinux && go run . mcp < requests.ndjson)
(cd apps/OpenComputerUseWindows && go run . mcp < requests.ndjson)
```

Then normalize `serverInfo.version` to `<VERSION>` and embed the response as the
case's `expect`.
