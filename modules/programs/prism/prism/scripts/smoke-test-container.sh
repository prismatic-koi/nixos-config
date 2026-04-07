#!/usr/bin/env bash
# smoke-test-container.sh — Verify that the prism-agent container image contains
# all required tools and that CA certificates work correctly.
#
# Usage:
#   ./scripts/smoke-test-container.sh [image:tag]
#
# Arguments:
#   image:tag  Optional. The container image to test (default: prism-agent:latest).
#
# Requirements:
#   - podman must be running and the target image must be loaded.
#     On Darwin, start the podman machine first:
#       podman machine start
#     Load the image (if not already loaded via the LaunchAgent):
#       podman load < result  # where result is from: nix build .#prismAgentImage
#
# The script exits 0 if every check passes, non-zero if any check fails.
# If podman is not in PATH the script exits 0 with a skip message, so it is
# safe to run in CI environments that do not have podman available.

set -euo pipefail

IMAGE="${1:-prism-agent:latest}"

# ── Preflight: skip gracefully if podman is unavailable ──────────────────────

if ! command -v podman &> /dev/null; then
  echo "smoke-test-container: podman not found in PATH — skipping smoke test" >&2
  exit 0
fi

# ── Helpers ──────────────────────────────────────────────────────────────────

PASS=0
FAIL=0

pass() {
  echo "  ✓ $1"
  PASS=$(( PASS + 1 ))
}

fail() {
  echo "  ✗ $1" >&2
  FAIL=$(( FAIL + 1 ))
}

run_check() {
  local label="$1"
  shift
  if podman run --rm "$IMAGE" "$@" > /dev/null 2>&1; then
    pass "$label"
  else
    fail "$label"
  fi
}

run_check_shell() {
  local label="$1"
  local cmd="$2"
  if podman run --rm "$IMAGE" bash -c "$cmd" > /dev/null 2>&1; then
    pass "$label"
  else
    fail "$label"
  fi
}

# ── Binary checks ─────────────────────────────────────────────────────────────

echo "smoke-test-container: testing image '$IMAGE'"
echo ""
echo "Binary checks:"

run_check "git"     git      --version
run_check "gh"      gh       --version
run_check "opencode" opencode --version
run_check "nix"     nix      --version
run_check "go"      go       version
run_check "curl"    curl     --version
run_check "jq"      jq       --version
run_check "rg"      rg       --version
run_check "prism"   prism    --version
run_check "python3" python3  --version
run_check "bun"     bun      --version
run_check "node"    node     --version
run_check "sops"    sops     --version
run_check "age"     age      --version

# ── CA certificate checks ─────────────────────────────────────────────────────

echo ""
echo "CA certificate checks:"

# Verify curl can reach GitHub over HTTPS — confirms the CA bundle is present
# and trusted. A failure here means the CA bundle is missing or misconfigured,
# which would break git, gh, and opencode's HTTPS fetches.
if podman run --rm "$IMAGE" curl --silent --fail https://github.com > /dev/null 2>&1; then
  pass "curl HTTPS to github.com succeeds (CA bundle trusted)"
else
  fail "curl HTTPS to github.com failed (CA bundle missing or misconfigured?)"
fi

# Verify NIX_SSL_CERT_FILE is set and points to an existing file inside the
# container. This validates the ENV entry in container-image.nix.
run_check_shell 'NIX_SSL_CERT_FILE is set and file exists' \
  'test -n "$NIX_SSL_CERT_FILE" && test -f "$NIX_SSL_CERT_FILE"'

# ── Nix wrapper check ─────────────────────────────────────────────────────────

echo ""
echo "Nix wrapper check:"

# The extraCommands block in container-image.nix replaces the real nix binary
# with a bash wrapper at /bin/nix and moves the original to /bin/.nix-real.
# Verify the wrapper file is a regular file (not a symlink to the store binary)
# and that the real binary is stashed alongside it.
run_check_shell "nix wrapper is a regular file at /bin/nix" \
  'test -f /bin/nix && ! test -L /bin/nix'
run_check_shell "real nix binary stashed at /bin/.nix-real" \
  'test -f /bin/.nix-real'

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"

if [ "$FAIL" -gt 0 ]; then
  echo "smoke-test-container: FAILED — ${FAIL} check(s) did not pass" >&2
  exit 1
fi

echo "smoke-test-container: all checks passed"
