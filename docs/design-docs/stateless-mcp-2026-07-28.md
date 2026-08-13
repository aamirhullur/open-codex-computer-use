# Stateless MCP 2026-07-28 design

Status: proposed for implementation

Owner: Open Computer Use maintainers

Last updated: 2026-08-13

Related execution plan: `docs/exec-plans/active/20260813-stateless-mcp-2026-07-28.md`

## Summary

Open Computer Use can support the stateless MCP core without rewriting its macOS, Linux, or Windows automation engines. The recommended change is a dual-era MCP adapter plus an explicit snapshot capability:

- modern requests use MCP `2026-07-28`, carry protocol metadata on every request, and never depend on `initialize`, `notifications/initialized`, or transport sessions;
- existing clients continue to use the current `2025-03-26` initialization flow;
- `get_app_state` mints an opaque `snapshot_ref` and every modern action call passes that reference explicitly;
- the permission-bearing device runtime owns the bounded snapshot store, while each MCP request is independently classifiable and dispatchable;
- the tool names and native automation implementations remain unchanged.

This is protocol-stateless, not application-stateless. A live accessibility tree and screenshot are application state. MCP 2026-07-28 explicitly permits such state when the server mints a handle and the client passes it back as an ordinary tool argument. The handle makes the dependency visible, bounded, and recoverable instead of hiding it in the stdio process lifetime.

## Decision

Implement a dual-era server that supports exactly these revisions in the first release:

1. `2026-07-28` with modern, per-request semantics.
2. `2025-03-26` with the repository's existing legacy handshake semantics.

Do not advertise `2025-11-25` until its complete wire behavior has been implemented and tested. A date string in an `initialize` result is not sufficient protocol support.

Keep stdio as the public transport. Streamable HTTP, OAuth, Tasks, and MRTR are outside this migration. The internal macOS Unix-domain-socket proxy remains an implementation detail and is not an MCP transport.

## Why this change is needed

### Protocol gap

The current MCP entry points are hand-written and implement the older lifecycle:

- `packages/OpenComputerUseKit/Sources/OpenComputerUseKit/MCPServer.swift`
- `apps/OpenComputerUseLinux/main.go`
- `apps/OpenComputerUseWindows/main.go`

Each accepts `initialize`, returns `2025-03-26`, ignores `notifications/initialized`, and serves later calls without per-request version or capability metadata. None implements `server/discover`, required `resultType`, cache hints, or result-level server identity.

### Hidden application-state gap

Action correctness currently depends on an implicit cache:

- Swift stores snapshots in `ComputerUseService.snapshotsByApp`.
- Linux and Windows store snapshots in `service.snapshots`.
- an action resolves its `element_index`, focused element, coordinate scale, PID, and target window from whichever snapshot is cached under the app name;
- if no snapshot exists, `currentSnapshot` silently captures a new one.

That last fallback is unsafe for a stateless call. Index `42` from one tree can identify a different element in a newly captured tree. It also makes success depend on whether two requests happen to share a process.

### Upstream baseline

The fork was five commits behind upstream. It was fast-forwarded locally from v0.3.0 (`a265277`) to upstream commit `ead48da` after confirming that v0.3.1 contains Linux accessibility and snapshot-boundary fixes but no MCP 2026-07-28 migration. This design therefore targets the current upstream architecture rather than duplicating existing work.

## Normative protocol behavior

### Era classification

The adapter classifies an stdio process once, based on how the client opens it:

| Opening message | Selected behavior |
| --- | --- |
| `server/discover` with modern `_meta` | modern |
| any other request with modern `_meta` | modern |
| `initialize` without modern `_meta` | legacy |
| ambiguous request without either form | reject; do not infer modern metadata |

After classification, a message from the other era is rejected. This prevents a single connection from accumulating implicit negotiation state while changing semantics between calls. The selected era is connection routing state for backward compatibility, not an MCP application session.

Modern-only clients may call any method directly. `server/discover` is optional for clients but mandatory for the server. A dual-era client can use it as the standard stdio compatibility probe and fall back to `initialize` when the probe receives a non-modern response.

### Modern request envelope

Every modern request must contain `params._meta` with:

- `io.modelcontextprotocol/protocolVersion: "2026-07-28"` — required;
- `io.modelcontextprotocol/clientCapabilities: { ... }` — required, and evaluated only for this request;
- `io.modelcontextprotocol/clientInfo` — recommended; validate it when present;
- `io.modelcontextprotocol/logLevel` — optional. The first implementation emits no MCP logging notifications, so it only validates and records this field.

Missing or malformed required metadata returns JSON-RPC `-32602` (`Invalid params`). An unsupported version returns `-32022` with:

```json
{
  "supported": ["2026-07-28", "2025-03-26"],
  "requested": "the-requested-version"
}
```

The legacy revision appears in discovery and error diagnostics because the server is dual-era. It is never accepted as per-request modern metadata; legacy semantics begin with `initialize`.

### Modern response envelope

Every successful modern result contains:

```json
{
  "resultType": "complete",
  "_meta": {
    "io.modelcontextprotocol/serverInfo": {
      "name": "open-computer-use",
      "version": "<build version>"
    }
  }
}
```

The protocol adapter adds these wire-only fields centrally after the handler returns. Tool-service code must not add them ad hoc. Legacy results remain byte-compatible and do not receive `resultType` or modern server metadata.

### `server/discover`

The modern response is deterministic and contains:

```json
{
  "resultType": "complete",
  "supportedVersions": ["2026-07-28", "2025-03-26"],
  "capabilities": {
    "tools": { "listChanged": false }
  },
  "instructions": "<computerUseServerInstructions>",
  "ttlMs": 300000,
  "cacheScope": "public",
  "_meta": {
    "io.modelcontextprotocol/serverInfo": {
      "name": "open-computer-use",
      "version": "<build version>"
    }
  }
}
```

`public` is safe because discovery, instructions, and the capability set do not vary by user. Five minutes limits stale metadata after a local binary upgrade while still avoiding repeated probes within a normal host lifetime.

### Supported methods

Modern mode supports `server/discover`, `tools/list`, and `tools/call`. It does not advertise or implement removed `ping`. `initialize` and `notifications/initialized` exist only in legacy mode.

`notifications/turn-ended` is a project-specific cleanup hook, not a standard MCP method. Preserve it as a best-effort custom notification in both eras, but never use it as the correctness boundary for snapshots. A dropped notification may leave the visual cursor visible until its existing timeout, but must not make later actions target stale state.

### Tool-list caching

Modern `tools/list` returns tools in the checked-in definition order and includes:

```json
{
  "resultType": "complete",
  "tools": [],
  "ttlMs": 300000,
  "cacheScope": "public"
}
```

The modern catalog is invariant across callers and requests, so `public` is appropriate. Legacy `tools/list` remains unchanged. Do not dynamically hide or reveal tools after an action.

## Explicit snapshot state

### Handle format and ownership

`get_app_state` returns a `snapshot_ref` formatted as:

```text
ocu_snapshot_v1_<base64url-encoded 24 random bytes>
```

The value is opaque, unguessable, and carries no user data. It is a capability reference, not authentication. Logs may include the `ocu_snapshot_v1_` prefix and the final six characters for correlation but never the full value.

The `SnapshotHandleStore` owns the mapping. On macOS it belongs to the long-lived permission-bearing app agent, not to an individual `AppAgentConnection` or `StdioMCPServer`. On Linux and Windows it initially belongs to the MCP process because those platforms do not yet have a separate device agent. That still conforms to the stateless protocol because the dependency is explicit; a process restart returns a recoverable expired/unknown-handle tool error instead of silently choosing new state.

If a remote or horizontally replicated MCP frontend is added later, it must route the handle to the same device agent or replace the local mapping with a shared store. Native accessibility objects must not be serialized into a browser-visible token.

### Stored record

Each live handle records:

- creation and absolute expiry timestamps;
- a monotonically increasing generation for the target window;
- normalized app name, bundle identifier or executable identity, and PID;
- stable window identifier where the platform provides one;
- window bounds, screenshot pixel dimensions, and target window layer;
- the rendered accessibility element records and their native backing references;
- snapshot mode (`real` or fixture) and capture options;
- lifecycle state: `live`, `in_flight`, `superseded`, or `expired`.

The initial limits are:

- 120-second absolute TTL, not sliding;
- one live generation per app/window target;
- at most 16 live targets per device runtime;
- at most 64 lightweight tombstones used to distinguish stale, expired, and unknown handles;
- least-recently-created eviction after removing expired records.

These values are constants with unit tests. They are not user configuration in the first release.

### Tool contract

Modern `get_app_state` returns the existing text and screenshot plus structured metadata:

```json
{
  "content": ["existing text and image blocks"],
  "structuredContent": {
    "snapshot_ref": "ocu_snapshot_v1_...",
    "captured_at": "2026-08-13T12:00:00Z",
    "expires_at": "2026-08-13T12:02:00Z",
    "generation": 7,
    "app": {
      "name": "Example",
      "bundle_identifier": "com.example.app",
      "pid": 1234
    },
    "window": {
      "id": "platform-window-id",
      "bounds": { "x": 0, "y": 0, "width": 1200, "height": 800 },
      "screenshot_pixels": { "width": 2400, "height": 1600 }
    }
  },
  "isError": false
}
```

The visible text block also prints `snapshot_ref` near the top. This redundancy lets models thread the handle even when a host does not expose `structuredContent` cleanly.

The modern tool catalog requires `snapshot_ref` for all action tools:

| Tool | Why the reference is required |
| --- | --- |
| `click` | binds element indices or screenshot pixels to one captured window |
| `perform_secondary_action` | binds an action name and native element |
| `scroll` | binds the scroll target element |
| `drag` | binds screenshot pixel scale and window bounds |
| `type_text` | binds the focused editable element and PID |
| `press_key` | binds keyboard delivery to the observed app/window/PID |
| `set_value` | binds the settable native element |

`list_apps` and `get_app_state` do not accept a handle. The legacy catalog stays unchanged and retains the current implicit-cache behavior during the compatibility window.

The catalog must be generated by `ToolCatalog.forEra(.modern | .legacy)` rather than by mutating a global definition after connection. Modern action descriptions say to pass the handle returned by the immediately preceding state or action result.

### Action transaction

Every modern action follows this sequence:

1. Parse and validate `snapshot_ref` before any input side effect.
2. Resolve the handle and reject unknown, expired, superseded, or already-in-flight records.
3. Verify the requested `app` resolves to the handle's app identity, PID, and window. A name alias may differ, but the resolved identity may not.
4. Under a per-target lock, compare the handle generation with the current generation and move it from `live` to `in_flight`.
5. Revalidate the minimum live properties needed by the action: process still exists, window identity still belongs to it, coordinates remain inside the captured screenshot, and the native element still exposes the requested role/action/settable property.
6. Dispatch exactly one native action using the stored snapshot. Never recapture and reinterpret the old index before dispatch.
7. Capture the post-action state, mint generation `n + 1`, mark the old handle `superseded`, and return the successor `snapshot_ref` in text and `structuredContent`.

If validation fails before dispatch, return a tool error and restore the handle to `live` when it is still safe to retry. If native dispatch begins but its outcome is uncertain, invalidate the handle and require `get_app_state`; do not retry automatically. If dispatch succeeds but refresh fails, supersede the old handle and return an error that explicitly instructs the caller to recapture.

This single-writer generation rule makes concurrent actions deterministic: one action acquires the current generation; another action using the same handle receives `snapshot_ref is in use` or `snapshot_ref is stale` and performs no input.

### Tool-level errors

Snapshot failures are ordinary `tools/call` results with `isError: true`, not JSON-RPC protocol errors. Use stable error identifiers in `structuredContent.error.code`:

- `snapshot_ref_missing`
- `snapshot_ref_malformed`
- `snapshot_ref_unknown`
- `snapshot_ref_expired`
- `snapshot_ref_stale`
- `snapshot_ref_in_use`
- `snapshot_target_changed`
- `snapshot_action_outcome_uncertain`

Every message tells the model whether it may retry the same handle or must call `get_app_state`.

## Component design

### Shared protocol concepts

Introduce the same concepts on all platforms:

- `ProtocolEra`: `legacy20250326` or `modern20260728`;
- `RequestEnvelope`: parsed version, client info, capabilities, and log level;
- `MCPRequestContext`: era plus envelope for one request;
- `ResponseDecorator`: adds modern `resultType` and server identity;
- `ToolCatalog`: deterministic era-specific definitions;
- `SnapshotHandleStore`: bounded state, target locks, generations, and tombstones;
- `SnapshotContext`: the explicit value passed from the dispatcher into service actions.

Keep wire parsing out of the automation service. Keep native automation out of the protocol adapter.

### Swift/macOS

Refactor `StdioMCPServer` into a thin decoder/router that accepts injected `ComputerUseToolDispatcher` and protocol metadata. `ToolCallResult` gains optional `structuredContent`; serialization of modern wire fields remains in the adapter.

`MacOSAppAgentRuntime` creates one shared automation runtime containing `ComputerUseService` and `SnapshotHandleStore`. Each `AppAgentConnection` creates only its protocol adapter and uses the shared runtime. Access to the Swift store is serialized by a dedicated queue or actor-compatible lock; native accessibility calls continue on the execution context required by the existing code.

Do not wait for the Swift MCP SDK. At the time of this design its published documentation still targets an older protocol, while this repository already has a small MCP surface. A focused hand-written adapter plus cross-platform golden fixtures is lower risk than importing a second process or partially supported SDK. Re-evaluate once the Swift SDK publishes stable 2026-07-28 support.

### Go/Linux and Go/Windows

First extract the duplicated JSON-RPC lifecycle and snapshot-handle behavior into a small internal Go package used by both binaries. Keep platform-specific automation in the existing app packages.

The official Go SDK supports 2026-07-28 and is a valid later replacement, but adopting it in the same slice would mix dependency migration with tool-state migration. The first implementation should use a small repository-owned adapter whose behavior is locked by the official schema fixtures and conformance suite. Record a follow-up decision after conformance passes.

### CLI batch calls

`open-computer-use call --calls` is not an MCP transport, but it currently relies on the same implicit cache. Extend its JSON call format to accept and forward `snapshot_ref`. For backward compatibility, sequential legacy-style batches may still omit it initially. Add an opt-in strict flag or internal modern mode in tests so the explicit-handle path is exercised without an MCP host.

## Compatibility and rollout

### Phase A: protocol foundation

- add era classification, request-envelope validation, `server/discover`, modern response decoration, and cache hints;
- preserve legacy behavior exactly;
- add raw JSON fixtures before changing snapshot semantics.

### Phase B: explicit snapshot path

- add the handle store and structured tool results;
- create era-specific tool schemas;
- require handles for modern actions;
- preserve legacy implicit lookup temporarily.

### Phase C: hardening

- add generation locking, tombstones, TTL/capacity enforcement, revalidation, and failure-state tests;
- run upstream MCP conformance for stdio `2026-07-28` and the repository's legacy smoke suite;
- verify at least one Tier 1 SDK client in modern-pinned and auto/fallback modes.

### Phase D: release and later cleanup

- release as v0.4.x with no MCP command/configuration change for users;
- document that modern hosts must pass `snapshot_ref` and that legacy hosts remain supported;
- collect telemetry from stderr-only debug logs;
- deprecate implicit legacy action state only in a later major release, after host compatibility is known.

Rollback is straightforward until Phase D: disable modern era classification and retain the legacy path. Snapshot-store changes must be additive until the release is validated.

## Testing strategy

### Protocol golden tests

Run the same newline-delimited JSON fixtures against Swift, Linux, and Windows handlers and compare normalized JSON:

- modern `server/discover` succeeds without `initialize`;
- direct modern `tools/list` succeeds and includes `resultType`, server identity, and public cache hints;
- direct modern `tools/call` succeeds with per-request metadata;
- missing version or capabilities returns `-32602`;
- unsupported version returns `-32022` with both supported revisions;
- modern mode rejects `initialize`, removed `ping`, and missing metadata;
- legacy initialization and subsequent calls retain their old shape;
- a modern probe can be followed by normal modern calls;
- tool order and discover output are deterministic.

### Snapshot-store tests

Use a fake clock, deterministic token source, and fake automation target to cover:

- mint, resolve, expiry, capacity eviction, and redacted logging;
- app/PID/window mismatch;
- coordinate binding at 1x and 2x screenshot scales;
- successful successor generation;
- stale and concurrent reuse with no second native action;
- pre-dispatch failure retaining a safe handle;
- uncertain dispatch invalidating it;
- refresh failure superseding it;
- process restart producing a recoverable unknown-handle error;
- all seven modern actions rejecting a missing handle before side effects.

### Integration and conformance

- keep the existing Swift and Go unit suites green;
- run `./scripts/smoke-all.sh` and platform-specific fixture flows;
- add a modern Swift smoke client or raw fixture driver alongside the existing legacy smoke client;
- run the official MCP conformance server suite for spec version `2026-07-28` against each supported platform entry point;
- test TypeScript SDK v2 in `modern` pin mode, `auto` mode against the dual-era server, and legacy mode;
- on macOS, close one socket connection and confirm a handle remains resolvable through the long-lived app agent; then restart the app agent and confirm the explicit recovery error.

## Acceptance criteria

The migration is complete when:

1. No modern request requires a prior request to determine protocol version, client capabilities, or server capabilities.
2. `server/discover` and every other modern result conform to the final 2026-07-28 schema.
3. Every modern action names the snapshot it intends to use, and no modern action silently captures a replacement snapshot before dispatch.
4. A stale, expired, concurrent, mismatched, or post-restart handle fails before unintended input.
5. Successful actions return a successor handle and state that can drive the next action.
6. Legacy `2025-03-26` clients still initialize and use all nine tools.
7. Tool catalogs are deterministic and cacheable with the documented scope and TTL.
8. Swift, Linux, and Windows pass shared protocol fixtures plus their platform tests.
9. The official 2026-07-28 server conformance suite passes, with any intentionally unsupported optional feature recorded.
10. Documentation no longer describes process memory as the only way to preserve `element_index` state.

## Security and privacy

- A snapshot may contain screen pixels and accessibility text. Never place that data inside the handle.
- Zero or release screenshot buffers and native element references when a record expires or is superseded.
- Bound memory and reject unbounded handle creation.
- Treat every handle received from the client as untrusted input and compare it in constant-time where practical.
- Keep the existing password-manager denylist and pointer-safety gates in front of snapshot capture and native actions.
- Do not log screenshot content, accessibility text, typed text, full handles, or client-supplied metadata values by default.
- Future remote HTTP exposure requires authentication, authorization, origin validation, and device routing; none is implied by this local stdio migration.

## Observability

Structured debug logs go to stderr only and include platform, era, method, result class, duration, target hash, generation, and redacted handle suffix. Add counters for minted, resolved, expired, stale, evicted, mismatched, concurrent, uncertain, and refresh-failed handles. Stdout remains JSON-RPC only.

## Alternatives considered

### Only remove `initialize`

Rejected. It would satisfy the superficial wire change while leaving element targeting dependent on hidden process state.

### Make every action capture a fresh snapshot

Rejected. An `element_index` and screenshot coordinate are meaningful only in the snapshot shown to the model. Recapturing can target a different element.

### Encode the entire snapshot in a signed token

Rejected for the first version. Screenshots, accessibility trees, and native element references are too large or non-serializable, and placing private UI content in client-visible tokens increases leakage risk.

### Introduce a Node/TypeScript sidecar

Rejected. It would add packaging, startup, IPC, and release complexity to three native binaries for a small protocol surface.

### Drop legacy support immediately

Rejected. Client adoption of MCP 2026-07-28 is still in transition, and stdio dual-era behavior is explicitly specified.

## Open questions to resolve during implementation

1. Confirm whether the official conformance runner can launch the macOS app-agent proxy directly or needs a small test-only stdio entry point.
2. Measure typical snapshot memory on all platforms; adjust the 16-target cap downward if it can exceed the reliability budget.
3. Decide whether `snapshot_ref` should also be exposed in an MCP content annotation for hosts that discard `structuredContent`; visible text remains the required fallback.
4. Decide when to make explicit handles mandatory in non-MCP `call --calls` batches.
5. Re-evaluate the official Swift and Go SDKs immediately before implementation, but do not change the behavior in this document without recording a decision in the execution plan.

## Primary references

- [MCP 2026-07-28 release](https://blog.modelcontextprotocol.io/posts/2026-07-28/)
- [MCP 2026-07-28 changelog](https://modelcontextprotocol.io/specification/2026-07-28/changelog)
- [Versioning and dual-era compatibility](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning)
- [`server/discover`](https://modelcontextprotocol.io/specification/2026-07-28/server/discover)
- [MCP 2026-07-28 schema](https://modelcontextprotocol.io/specification/2026-07-28/schema)
- [SEP-2575: Make MCP Stateless](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/main/seps/2575-stateless-mcp.md)
- [SEP-2567: Sessionless MCP](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/main/seps/2567-sessionless-mcp.md)
- [TypeScript SDK migration guide](https://ts.sdk.modelcontextprotocol.io/v2/migration/support-2026-07-28)
- [MCP conformance suite](https://github.com/modelcontextprotocol/conformance)
