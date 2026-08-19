#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cd "${repo_root}"

swift build
OPEN_COMPUTER_USE_VISUAL_CURSOR=0 ".build/debug/OpenComputerUseSmokeSuite"
".build/debug/OpenComputerUseSmokeSuite" --cursor-idle-only
# Modern 2026-07-28 pass: same env (proxy disabled + fixture headless are set by the
# suite itself; visual cursor forced off here). set -e fails the script if it fails.
OPEN_COMPUTER_USE_VISUAL_CURSOR=0 ".build/debug/OpenComputerUseSmokeSuite" --modern
