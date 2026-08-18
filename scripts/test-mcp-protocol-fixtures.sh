#!/usr/bin/env bash

# Runs the cross-platform MCP protocol golden fixtures (slice M0):
#   - the Swift XCTest fixture runner (macOS handler);
#   - the Go fixture runners in each app directory (Linux and Windows handlers);
#   - a subprocess guard asserting the Swift stdio server writes only JSON to
#     stdout (no diagnostic ever leaks onto the JSON-RPC channel). The guard's
#     input is derived from the shared legacy fixtures, so it exercises exactly
#     the requests that are frozen.
#
# Swift, Go, and python3 steps are each guarded by command -v so the script
# degrades on a toolchain-limited host, matching scripts/ci.sh.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

run_swift_suite() {
  echo "swift: MCP protocol fixtures"
  (
    cd "${repo_root}"
    swift test --filter MCPProtocolFixtureTests
  )
}

run_go_suites() {
  echo "go: shared protocol unit tests (packages/go-mcp)"
  (
    cd "${repo_root}/packages/go-mcp"
    go test -count=1 ./...
  )
  for app in OpenComputerUseWindows OpenComputerUseLinux; do
    echo "go: MCP protocol fixtures (${app})"
    (
      cd "${repo_root}/apps/${app}"
      go test -count=1 -run '^TestMCPProtocolFixtures' ./...
    )
  done
}

run_stdout_json_guard() {
  local binary="${repo_root}/.build/debug/OpenComputerUse"
  if [ ! -x "${binary}" ]; then
    echo "swift: building OpenComputerUse product for the stdout JSON guard"
    (
      cd "${repo_root}"
      swift build --product OpenComputerUse
    )
  fi

  # Script-global (not local) so the EXIT trap can still see them when it fires
  # at top-level scope; the :- defaults keep set -u happy if the guard is skipped.
  guard_requests="$(mktemp)"
  guard_output="$(mktemp)"
  # EXIT (not RETURN) so a set -e abort mid-guard still removes the tempfiles.
  trap 'rm -f "${guard_requests:-}" "${guard_output:-}"' EXIT

  # Derive the piped requests from the shared legacy fixtures so the guard
  # exercises exactly what is frozen there (ping, the tool errors, unknown
  # method, the parse-error line, and the notifications), in filename order.
  # The per-platform initialize and tools/list cases live under legacy/<platform>/
  # and are covered by the in-process runners.
  python3 - "${repo_root}/tests/mcp-protocol-fixtures/legacy" > "${guard_requests}" <<'PY'
import json
import os
import sys

legacy_dir = sys.argv[1]
for name in sorted(os.listdir(legacy_dir)):
    if not name.endswith(".json"):
        continue
    path = os.path.join(legacy_dir, name)
    if not os.path.isfile(path):
        continue
    with open(path) as handle:
        case = json.load(handle)
    for step in case.get("steps", []):
        if "request_raw" in step:
            print(step["request_raw"])
        elif "request" in step:
            print(json.dumps(step["request"], separators=(",", ":")))
PY

  echo "swift: stdout JSON guard"
  env OPEN_COMPUTER_USE_DISABLE_APP_AGENT_PROXY=1 OPEN_COMPUTER_USE_VISUAL_CURSOR=0 \
    "${binary}" mcp < "${guard_requests}" > "${guard_output}"

  python3 - "${guard_output}" <<'PY'
import json
import sys

path = sys.argv[1]
count = 0
with open(path) as handle:
    for lineno, line in enumerate(handle, 1):
        stripped = line.strip()
        if not stripped:
            continue
        try:
            json.loads(stripped)
        except ValueError as error:
            print(f"non-JSON stdout line {lineno}: {stripped!r} ({error})")
            sys.exit(1)
        count += 1

if count == 0:
    print("stdout JSON guard produced no output lines")
    sys.exit(1)

print(f"stdout JSON guard: OK ({count} JSON lines)")
PY
}

if command -v swift >/dev/null 2>&1; then
  run_swift_suite
  if command -v python3 >/dev/null 2>&1; then
    run_stdout_json_guard
  else
    echo "python3 not found; skipping stdout JSON guard"
  fi
else
  echo "swift not found; skipping Swift fixture suite and stdout JSON guard"
fi

if command -v go >/dev/null 2>&1; then
  run_go_suites
else
  echo "go not found; skipping Go fixture suites"
fi

echo "MCP protocol fixtures passed"
