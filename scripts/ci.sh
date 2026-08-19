#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

"${repo_root}/scripts/check-docs.sh"
"${repo_root}/scripts/check-repo-hygiene.sh"
"${repo_root}/scripts/check-action-pinning.sh"

while IFS= read -r file; do
  bash -n "$file"
done < <(find "${repo_root}/scripts" -type f -name '*.sh' | sort)

while IFS= read -r file; do
  node --check "$file"
done < <(find "${repo_root}/scripts" -type f -name '*.mjs' | sort)

(
  cd "${repo_root}/apps/OpenComputerUseLinux"
  python3 -m unittest -v runtime_test.py
)

if command -v go >/dev/null 2>&1; then
  (
    cd "${repo_root}/packages/go-mcp"
    go test -count=1 ./...
  )
  (
    cd "${repo_root}/apps/OpenComputerUseWindows"
    go test -count=1 ./...
  )
  (
    cd "${repo_root}/apps/OpenComputerUseLinux"
    go test -count=1 ./...
  )
fi

"${repo_root}/scripts/test-mcp-protocol-fixtures.sh"

# Attempt the official MCP conformance suite. The script is skip-guarded (npx +
# network) and degrades to exit 0 with a loud banner when it cannot run, so it
# never breaks an offline or toolchain-limited CI run.
"${repo_root}/scripts/test-mcp-conformance.sh"

echo "基础 CI 检查通过"
