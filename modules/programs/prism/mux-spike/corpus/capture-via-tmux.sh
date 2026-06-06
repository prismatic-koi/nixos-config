#!/usr/bin/env bash
# capture-via-tmux.sh — apples-to-apples corpus walk under tmux.
#
# Drives the same six apps as `mux-spike corpus` under a fresh tmux server
# (using $TMUX_TMPDIR to avoid colliding with the user's running tmux) and
# writes captures to the same --out tree, under <app>/tmux/. Diff target:
#
#   diff -ru <out>/<app>/xvt/cells.ansi <out>/<app>/tmux/cells.ansi
#
# Keystroke shorthand is parsed inline (the same vocabulary as the Go
# driver; the spike does not import the Go helper from bash to keep this
# script auditable on its own).

set -uo pipefail

OUT="${OUT:-/tmp/mux-spike-report}"
MANIFEST="${MANIFEST:-corpus/corpus.toml}"
ONLY="${ONLY:-}"
SOCKET_NAME="mux-spike-$$"

# Parse --out FOO / --manifest FOO / --only A,B from argv.
while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)       OUT="$2"; shift 2 ;;
    --manifest)  MANIFEST="$2"; shift 2 ;;
    --only)      ONLY="$2"; shift 2 ;;
    -h|--help)
      echo "usage: $0 [--out DIR] [--manifest PATH] [--only A,B,...]"
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

command -v tmux >/dev/null || { echo "tmux not found in PATH" >&2; exit 1; }
[[ -f "$MANIFEST" ]] || { echo "manifest not found: $MANIFEST" >&2; exit 1; }

mkdir -p "$OUT"

# Isolate from the user's running tmux by giving us our own socket.
TMUX_SOCK="$(mktemp -u "${TMPDIR:-/tmp}/tmux-mux-spike-XXXXXX")"
trap 'tmux -S "$TMUX_SOCK" kill-server 2>/dev/null || true; rm -f "$TMUX_SOCK"' EXIT

T() { tmux -S "$TMUX_SOCK" "$@"; }

in_only() {
  local name="$1"
  [[ -z "$ONLY" ]] && return 0
  local IFS=,
  for n in $ONLY; do
    [[ "$n" == "$name" ]] && return 0
  done
  return 1
}

# Translate "<esc>:wq<cr>" → tmux send-keys vocabulary.
#
# Strategy: emit a flat send-keys argv where bracketed tokens become tmux's
# named keys (Escape, Enter, Tab, BSpace, F5, ...) and ctrl-tokens become
# "C-X". Free text becomes a single -l literal arg so we don't have to
# escape inside it.
send_keystroke() {
  local target="$1"; shift
  local s="$1"
  local i=0
  local buf=""
  local n=${#s}
  while (( i < n )); do
    local c="${s:i:1}"
    if [[ "$c" != "<" ]]; then
      buf+="$c"
      i=$((i + 1))
      continue
    fi
    # find closing >
    local rest="${s:i}"
    local close="${rest%%>*}"
    if [[ "$close" == "$rest" ]]; then
      buf+="$c"
      i=$((i + 1))
      continue
    fi
    # flush buf
    if [[ -n "$buf" ]]; then
      T send-keys -t "$target" -l -- "$buf"
      buf=""
    fi
    local tok="${close}>"
    local lower="${tok,,}"
    local key=""
    case "$lower" in
      "<esc>")   key=Escape   ;;
      "<cr>")    key=Enter    ;;
      "<lf>")    key=Enter    ;;
      "<tab>")   key=Tab      ;;
      "<bs>")    key=BSpace   ;;
      "<space>") key=Space    ;;
      "<up>")    key=Up       ;;
      "<down>")  key=Down     ;;
      "<left>")  key=Left     ;;
      "<right>") key=Right    ;;
      "<f1>")    key=F1       ;;
      "<f2>")    key=F2       ;;
      "<f3>")    key=F3       ;;
      "<f4>")    key=F4       ;;
      "<f5>")    key=F5       ;;
      "<f6>")    key=F6       ;;
      "<f7>")    key=F7       ;;
      "<f8>")    key=F8       ;;
      "<f9>")    key=F9       ;;
      "<f10>")   key=F10      ;;
      "<f11>")   key=F11      ;;
      "<f12>")   key=F12      ;;
      "<c-"?">")
        local ch="${lower:3:1}"
        key="C-${ch}"
        ;;
      *)
        # Unknown token — forward literally so the operator sees it.
        T send-keys -t "$target" -l -- "$tok"
        i=$((i + ${#tok}))
        continue
        ;;
    esac
    T send-keys -t "$target" "$key"
    i=$((i + ${#tok}))
  done
  if [[ -n "$buf" ]]; then
    T send-keys -t "$target" -l -- "$buf"
  fi
}

# Yield rows of MANIFEST as: name|cols|rows|settle_ms|post_settle_ms|argv_json|triggers_json
# using python for parsing (bash + toml is a footgun we don't need).
python3 - "$MANIFEST" <<'PY' >/tmp/mux-spike-manifest.tsv
import sys, json, tomllib
with open(sys.argv[1], 'rb') as f:
    m = tomllib.load(f)
for a in m.get('apps', []):
    print('\t'.join([
        a['name'],
        str(a.get('cols', 120)),
        str(a.get('rows', 40)),
        str(a.get('settle_ms', 500)),
        str(a.get('post_trigger_settle_ms', 0)),
        json.dumps(a.get('argv', [])),
        json.dumps(a.get('triggers', [])),
    ]))
PY

while IFS=$'\t' read -r name cols rows settle post argv_json triggers_json; do
  if ! in_only "$name"; then
    continue
  fi
  echo "[tmux] $name ..."
  appdir="$OUT/$name/tmux"
  mkdir -p "$appdir"

  # Build a temp launcher script so we don't have to nest single quotes
  # through bash → tmux → sh. The launcher runs the manifest argv, then
  # — if the app exits quickly — keeps the pane alive on `sleep infinity`
  # so the session survives long enough for capture-pane to run.
  #
  # No shebang — we invoke it via `sh PATH` from tmux to avoid the NixOS
  # /usr/bin/env trap.
  launcher="$(mktemp "${TMPDIR:-/tmp}/mux-spike-launch-XXXXXX.sh")"
  python3 - "$launcher" "$argv_json" <<'PY'
import json, shlex, sys
path, argv_json = sys.argv[1], sys.argv[2]
argv = json.loads(argv_json)
with open(path, 'w') as f:
    f.write(shlex.join(argv) + '\n')
    f.write('exec sleep infinity\n')
PY

  session="mux-spike-$name"
  # -d detached. -x/-y set the pane size. We invoke the launcher via
  # `sh PATH` so the pane's pid 1 is sh and the launcher runs as a sequence
  # of commands. Trailing `sleep infinity` (inside the launcher) keeps the
  # pane alive after the app exits so capture-pane has something to read.
  T new-session -d -s "$session" -x "$cols" -y "$rows" sh "$launcher" \
    || { echo "  new-session failed" >&2; rm -f "$launcher"; continue; }

  # Disable status bar — we are scoring the pane content, not tmux chrome.
  T set-option -t "$session" status off >/dev/null 2>&1 || true

  sleep "$(awk "BEGIN{print $settle/1000}")"

  # Drive triggers.
  trig_count=$(python3 -c 'import json,sys; print(len(json.loads(sys.argv[1])))' "$triggers_json")
  if (( trig_count > 0 )); then
    while IFS= read -r trig; do
      send_keystroke "$session" "$trig"
      sleep 0.08
    done < <(python3 -c 'import json,sys
for t in json.loads(sys.argv[1]): print(t)' "$triggers_json")
  fi

  sleep "$(awk "BEGIN{print $post/1000}")"

  # capture-pane -e preserves escape sequences. -p prints to stdout.
  T capture-pane -t "$session" -e -p > "$appdir/cells.ansi" \
    || echo "  capture-pane failed" >&2
  # Plain-text variant for the cheap diff.
  T capture-pane -t "$session" -p > "$appdir/cells.txt" \
    || true

  cat > "$appdir/meta.json" <<JSON
{
  "app": "$name",
  "cols": $cols,
  "rows": $rows,
  "backend": "tmux",
  "settle_ms": $settle,
  "post_trigger_settle_ms": $post
}
JSON

  T kill-session -t "$session" >/dev/null 2>&1 || true
  rm -f "$launcher"

done </tmp/mux-spike-manifest.tsv

echo "[tmux] done. captures under $OUT/*/tmux/"
