#!/usr/bin/env bash
# claude-xdg-migrate.sh — one-time idempotent migration of claude-code state
# from the old canonical locations (~/.claude/ and ~/.claude.json) to the XDG
# config dir reached via CLAUDE_CONFIG_DIR (~/.config/claude).
#
# Issue #2243 (Step 3c of #2132). Invoked from the home-manager activation
# script in modules/programs/prism/claude-code.nix; takes explicit paths so
# the logic is unit-testable without touching a real $HOME (see
# internal/integration/claude_xdg_migration_script_test.go in the prism Go
# tree).
#
# Usage: claude-xdg-migrate.sh <src-dir> <src-json> <dst-dir>
#   e.g. claude-xdg-migrate.sh "$HOME/.claude" "$HOME/.claude.json" "$HOME/.config/claude"
#
# Behaviour (all idempotent):
#   - Moves history.jsonl, projects/, plugins/, telemetry/, backups/ from
#     <src-dir> into <dst-dir>, and <src-json> to <dst-dir>/.claude.json.
#   - No source state at all → exit 0 without creating <dst-dir>.
#   - A source entry that is absent (e.g. already migrated) → skipped.
#   - A destination entry that already exists → skip-and-warn, NEVER
#     overwritten; the source entry is left in place.
#   - Entries are moved one by one (never the <src-dir> itself — it may be a
#     mount point under impermanence). mv falls back to copy+remove across
#     filesystems (EXDEV), which covers persisted bind mounts.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <src-dir> <src-json> <dst-dir>" >&2
  exit 64
fi

src_dir=$1
src_json=$2
dst_dir=$3

entries=(history.jsonl projects plugins telemetry backups)

# exists: true for any filesystem entry, including dangling symlinks
# (-e follows symlinks and reports false for dangling ones; -L catches those).
exists() {
  [ -e "$1" ] || [ -L "$1" ]
}

# Nothing to migrate when no source state exists — exit before creating the
# destination so an absent ~/.claude stays a strict no-op.
have_source=false
if exists "$src_json"; then
  have_source=true
elif [ -d "$src_dir" ]; then
  for entry in "${entries[@]}"; do
    if exists "$src_dir/$entry"; then
      have_source=true
      break
    fi
  done
fi
if [ "$have_source" = false ]; then
  exit 0
fi

mkdir -p "$dst_dir"

# migrate <src> <dst>: move one entry, skipping (with a warning) when the
# destination already exists. Never overwrites.
migrate() {
  local src=$1 dst=$2
  if ! exists "$src"; then
    return 0 # absent or already migrated — no-op
  fi
  if exists "$dst"; then
    echo "claude-xdg-migrate: SKIP $src -> $dst (destination already exists; not overwritten)" >&2
    return 0
  fi
  mv "$src" "$dst"
  echo "claude-xdg-migrate: moved $src -> $dst" >&2
}

if [ -d "$src_dir" ]; then
  for entry in "${entries[@]}"; do
    migrate "$src_dir/$entry" "$dst_dir/$entry"
  done
fi

migrate "$src_json" "$dst_dir/.claude.json"
