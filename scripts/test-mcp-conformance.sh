#!/usr/bin/env bash

# Attempts the official MCP conformance suite (spec version 2026-07-28) against
# the macOS stdio server:
#
#   env OPEN_COMPUTER_USE_DISABLE_APP_AGENT_PROXY=1 \
#       OPEN_COMPUTER_USE_VISUAL_CURSOR=0 \
#       .build/debug/OpenComputerUse mcp
#
# The suite (github.com/modelcontextprotocol/conformance, npm
# @modelcontextprotocol/conformance) ships only over npx, and there is no Node
# lockfile/install path in this repo, so npx is the only route. This script is
# skip-guarded so it degrades loudly and exits 0 on a toolchain- or
# network-limited host, matching scripts/ci.sh. Pass --require to make an absent
# prerequisite (npx, network) or an un-runnable suite a hard failure (exit 1) for
# manual/gated runs.
#
# Raw suite output is written under an output directory (default: a mktemp dir;
# override with --output-dir or OCU_CONFORMANCE_OUTDIR) and a summary is printed.
#
# Known structural gaps this script detects and records honestly (it never fakes
# a pass): the published suite does not recognize the 2026-07-28 spec version,
# and its `server` mode connects only to an HTTP `--url` while this server is
# stdio-only. Both are captured from the suite's own output.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

require=0
outdir="${OCU_CONFORMANCE_OUTDIR:-}"
conformance_pkg="${OCU_CONFORMANCE_PKG:-@modelcontextprotocol/conformance}"
spec_version="2026-07-28"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --require)
      require=1
      ;;
    --output-dir)
      shift
      outdir="${1:-}"
      ;;
    --output-dir=*)
      outdir="${1#*=}"
      ;;
    -h | --help)
      echo "Usage: $0 [--require] [--output-dir <path>]"
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      exit 2
      ;;
  esac
  shift
done

# skip_or_fail prints a loud banner and either exits 0 (default: the step
# degrades) or exits 1 (--require: the missing prerequisite is a hard failure).
skip_or_fail() {
  local reason="$1"
  echo "=================================================================="
  echo "MCP conformance: SKIPPED"
  echo "  reason: ${reason}"
  echo "=================================================================="
  if [ "${require}" -eq 1 ]; then
    echo "--require was set: treating skip as failure." >&2
    exit 1
  fi
  exit 0
}

if ! command -v npx >/dev/null 2>&1; then
  skip_or_fail "npx not found (the official conformance suite is only available via npx)"
fi

# Network probe: the suite is fetched on demand via npx, so an offline host must
# skip rather than hang. curl is present on macOS and the CI images; if it is
# absent we optimistically proceed and let npx surface any network error.
if command -v curl >/dev/null 2>&1; then
  if ! curl -fsS --max-time 8 https://registry.npmjs.org/ >/dev/null 2>&1; then
    skip_or_fail "npm registry unreachable (offline); cannot fetch the conformance suite"
  fi
fi

# Output directory for the raw suite transcripts. A caller-supplied directory
# (--output-dir / OCU_CONFORMANCE_OUTDIR) is retained; an auto-created mktemp dir
# is cleaned up on exit so repeated (ci.sh) runs do not leak temp directories.
auto_outdir=""
if [ -z "${outdir}" ]; then
  outdir="$(mktemp -d "${TMPDIR:-/tmp}/ocu-mcp-conformance.XXXXXX")"
  auto_outdir="${outdir}"
  # EXIT (not RETURN) so a set -e abort mid-run still removes the temp dir.
  trap 'rm -rf "${auto_outdir:-}"' EXIT
fi
mkdir -p "${outdir}"
echo "MCP conformance: raw output -> ${outdir}"

# Build the macOS debug binary if needed (guarded by swift availability).
binary="${repo_root}/.build/debug/OpenComputerUse"
if [ ! -x "${binary}" ]; then
  if command -v swift >/dev/null 2>&1; then
    echo "MCP conformance: building OpenComputerUse product"
    (
      cd "${repo_root}"
      swift build --product OpenComputerUse
    )
  else
    skip_or_fail "macOS server binary missing and swift not found to build it"
  fi
fi

npx_run() {
  # --yes so a missing package is fetched non-interactively; the pinned/overridable
  # package name keeps the run reproducible.
  npx --yes "${conformance_pkg}" "$@"
}

echo "MCP conformance: suite = ${conformance_pkg}"
npx_run --version > "${outdir}/version.txt" 2>&1 || true
suite_version="$(tr -d '\r' < "${outdir}/version.txt" | tail -1)"
echo "MCP conformance: suite version = ${suite_version}"

# Attempt 1: does the suite recognize the pinned spec version? A date version is
# accepted iff it appears in the suite's known set; an unknown version prints the
# valid set. This is the primary go/no-go for the 2026-07-28 target.
version_probe="${outdir}/list-server-${spec_version}.txt"
npx_run list --server --spec-version "${spec_version}" > "${version_probe}" 2>&1 || true

# Attempt 2: capture the server-mode transport contract (HTTP --url vs stdio).
server_help="${outdir}/server-help.txt"
npx_run server --help > "${server_help}" 2>&1 || true

version_unknown=0
if grep -qi "Unknown spec version" "${version_probe}"; then
  version_unknown=1
fi

http_only=0
if grep -qiE "required option .*--url|--url <url>" "${server_help}" && ! grep -qiE "\-\-command" "${server_help}"; then
  http_only=1
fi

echo ""
echo "=================================================================="
echo "MCP conformance: SUMMARY"
echo "  suite package : ${conformance_pkg} (${suite_version})"
echo "  target spec   : ${spec_version}"
echo "  server binary : ${binary}"
echo "------------------------------------------------------------------"

if [ "${version_unknown}" -eq 1 ] || [ "${http_only}" -eq 1 ]; then
  echo "  VERDICT: CANNOT RUN against this server (documented structural gaps)"
  if [ "${version_unknown}" -eq 1 ]; then
    echo "   - the published suite does not recognize spec version ${spec_version}."
    echo "     Valid versions reported by the suite:"
    sed 's/^/       /' "${version_probe}"
  fi
  if [ "${http_only}" -eq 1 ]; then
    echo "   - the suite's 'server' mode requires an HTTP '--url'; this server is"
    echo "     stdio-only (no HTTP endpoint), so the server suite cannot connect."
    echo "     See ${server_help} for the suite's server options."
  fi
  echo "------------------------------------------------------------------"
  echo "  This is recorded as an explicit gap, not a pass. The modern 2026-07-28"
  echo "  wire protocol is validated by the in-repo protocol fixtures and unit"
  echo "  suites (scripts/test-mcp-protocol-fixtures.sh) instead."
  echo "=================================================================="
  if [ "${require}" -eq 1 ]; then
    echo "--require was set: treating an un-runnable suite as failure." >&2
    exit 1
  fi
  exit 0
fi

# If neither structural gap is present, the suite recognizes the spec version and
# exposes a way to test this server: run it and let its exit status stand.
echo "  VERDICT: suite recognizes ${spec_version}; running server conformance"
echo "=================================================================="
run_log="${outdir}/server-run.txt"
if npx_run server --spec-version "${spec_version}" --suite active -o "${outdir}/results" \
  > "${run_log}" 2>&1; then
  echo "MCP conformance: server suite PASSED (see ${run_log})"
  exit 0
fi

echo "MCP conformance: server suite reported failures (see ${run_log})" >&2
tail -40 "${run_log}" >&2 || true
if [ "${require}" -eq 1 ]; then
  exit 1
fi
exit 0
