# Open Computer Use Troubleshooting

Read this reference when setup, permission checks, app discovery, snapshots, or actions fail.

## Unsupported macOS Version

The macOS runtime requires macOS 14.0 or later. Check the host version before troubleshooting permissions:

```sh
sw_vers -productVersion
```

On macOS versions earlier than 14.0, the binary cannot launch and may report a `dyld` or minimum-version incompatibility. `open-computer-use doctor`, Accessibility authorization, and Screen Recording authorization cannot resolve this error. Upgrade macOS or run Open Computer Use on a supported macOS, Windows, or Linux desktop.

## First Checks

Start with:

```sh
open-computer-use -h
open-computer-use doctor
open-computer-use call list_apps
```

On macOS 14.0 or later, `doctor` reports Accessibility and Screen Recording status. If either is missing, ask the user to approve the onboarding UI.

## App Not Found

If `get_app_state` cannot find an app:

1. Run `open-computer-use call list_apps`.
2. Use the app name or bundle identifier from that result.
3. Confirm the app is running and has a visible, non-minimized window.
4. On macOS, rerun `open-computer-use doctor`.

Do not silently switch to a different app when the requested target is not available.

## Empty Or Missing Snapshot

Common causes:

- The app has no visible window.
- The window is minimized, hidden, or on an unavailable desktop.
- macOS Screen Recording permission is missing.
- Windows or Linux commands are running outside the logged-in desktop session.
- Linux screenshot support is blocked by the compositor or desktop portal state.

Ask the user to bring the target app/window into a visible state when automation cannot do so safely.

## Truncated Text

Snapshot text is limited to 500 characters by default. If a visible chat message, email body, document paragraph, or form value ends with `...`, do not assume the page itself is missing content.

Request a larger or unlimited text limit explicitly:

```sh
open-computer-use call get_app_state --args '{"app":"TextEdit","text_limit":1000}'
open-computer-use call get_app_state --args '{"app":"TextEdit","text_limit":"max"}'
open-computer-use snapshot --text-limit 1000 TextEdit
open-computer-use snapshot --text-limit max TextEdit
```

`text_limit: "max"` only disables the text character limit. It does not remove the default 1200 node count limit, 64 level tree depth limit, screenshot size, permission, or desktop-session protections.

## Incomplete Long Pages Or Lists

If the screenshot clearly shows more visible content than the accessibility tree returns, the snapshot may have hit its default 1200 node or 64 level tree budget. Do not treat that as proof that the page failed to load.

Request a larger tree budget explicitly:

```sh
open-computer-use call get_app_state --args '{"app":"Google Chrome","max_tree_nodes":3000,"max_tree_depth":96}'
open-computer-use snapshot --max-tree-nodes 3000 --max-tree-depth 96 "Google Chrome"
```

Increasing the tree budget does not change text truncation, screenshot limits, permissions, or desktop-session requirements.

## Element Action Fails

If an element-targeted action fails:

1. Re-run `get_app_state`.
2. Confirm the `element_index` still exists and refers to the intended UI element.
3. Prefer `set_value` for settable text/value controls.
4. Prefer `perform_secondary_action` only for actions exposed in the state result.
5. Use coordinate `click`, `scroll`, or `drag` only after the semantic route is unavailable.

## Snapshot Reference Errors (Modern MCP)

Modern MCP `2026-07-28` hosts thread an explicit `snapshot_ref` from `get_app_state` into each action. When an action returns `isError: true` with a `structuredContent.error.code`, use the code to decide between recapturing and retrying:

- `snapshot_ref_missing` / `snapshot_ref_malformed`: the action was called without a valid handle. Call `get_app_state` and pass the returned `snapshot_ref` (also printed near the top of the visible text) into the action.
- `snapshot_ref_unknown` / `snapshot_ref_expired`: the handle is gone (past its 120-second TTL, or the runtime restarted). Recapture with `get_app_state` and use the fresh handle. Do not retry the old one.
- `snapshot_ref_stale`: a newer generation for this target exists (a successful action already superseded this handle). Use the successor handle returned by the last successful result, or recapture.
- `snapshot_ref_in_use`: another action is already running against this handle. This is a concurrency guard, not a target change. Wait for that action to finish and use its successor handle; no input was performed.
- `snapshot_target_changed`: the app/PID/window or element no longer matches what was captured. No input was performed. Recapture with `get_app_state`.
- `snapshot_action_outcome_uncertain`: dispatch started but its result could not be confirmed. Do not retry automatically. Recapture with `get_app_state` and re-observe before acting.

Rule of thumb: `_in_use` means wait and reuse the successor; `_stale` means use the successor handle; every other code means recapture with `get_app_state`. Legacy `2025-03-26` hosts do not pass handles and do not see these codes.

## Desktop Session Issues

Windows UI Automation and Linux AT-SPI require a live user desktop. SSH sessions, CI jobs, launch daemons, or services often do not have access to the GUI session even when the CLI binary starts successfully.

If desktop access is missing, ask the user to run the command from the logged-in desktop session or start the target app visibly in that session.

## Linux Accessible Interface Errors

If a Linux release reports that `Accessible` has no `is_text` or `is_editable_text` attribute, upgrade to a release containing the AT-SPI interface-detection fix. Current source checks `Accessible.get_interfaces()` for `Text` and `EditableText`, which is compatible with the PyGObject bindings used by Ubuntu 24.04.

## Permission And Safety Issues

- Do not bypass macOS TCC prompts.
- Do not enable global pointer fallbacks unless the user asks for low-level diagnostic behavior.
- Do not interact with password managers or sensitive apps unless the user explicitly requests it.
- Pause before submitting, sending, deleting, purchasing, or approving anything externally visible.
