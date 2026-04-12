# forgecode zsh integration
# Loaded from modules/programs/prism/forgecode.nix via initContent at order 1100
# (after base zsh init at 1000).
#
# Concurrency model:
#   - Plugin content cache: shared across all shells on this host, keyed on
#     forge binary path + version. Safe because the content is deterministic
#     per forge binary (forge embeds its shell plugin via include_dir! and
#     emits the same bytes on every invocation for a given build).
#   - rprompt cache: per-shell, keyed on a unique shell-session ID
#     (_FORGE_SHELL_ID), so N concurrent shells don't stomp on each other's
#     prompt state.
#
# CONTAINER NOTE (future work): If ~/ is bind-mounted into containers, host
# shells and container shells share the same cache dir. The shell-session ID
# (PID + microsecond timestamp) prevents filename collisions in practice but
# does NOT protect against PID-namespace aliasing in edge cases. When
# container support is wired up, extend _FORGE_SHELL_ID to include a namespace
# identifier (e.g. from /proc/self/cgroup, /run/.containerenv, or a
# FORGE_RPROMPT_NAMESPACE env var injected by the container runtime). See the
# forgecode.nix module for related notes on the ~/forge/ state directory
# itself.
#
# Note on registration with p10k: the `forge` segment is appended to
# POWERLEVEL9K_RIGHT_PROMPT_ELEMENTS statically in
# modules/programs/files/p10k.zsh, NOT from this file. home-manager sources
# p10k plugins in a separate section of .zshrc that runs AFTER all initContent
# blocks, so at the moment this file executes the right-prompt array does not
# yet exist. Function definitions (prompt_forge / instant_prompt_forge) made
# here persist across that later section, so p10k picks them up correctly
# when it finally renders.

zmodload zsh/datetime 2>/dev/null
zmodload zsh/stat     2>/dev/null

# ---- section 1: cached plugin loader ----
# Hash-keyed cache of `forge zsh plugin` output. Shared across all shells on
# this host since the content is deterministic per forge binary.
_forge_plugin_init() {
  [[ -n "$_FORGE_PLUGIN_LOADED" ]] && return 0

  local forge_bin cache_dir plugin_cache key_file current_key
  forge_bin=${FORGE_BIN:-forge}
  command -v "$forge_bin" >/dev/null 2>&1 || return 0

  cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}/forgecode"
  plugin_cache="$cache_dir/plugin.zsh"
  key_file="$cache_dir/plugin.key"

  # Key: resolved absolute path + version. Nix store path changes on every
  # forgecode bump, so this automatically invalidates the cache.
  current_key="$(readlink -f "$(command -v "$forge_bin")"):$("$forge_bin" --version 2>/dev/null)"

  if [[ ! -f "$plugin_cache" || ! -f "$key_file" || "$(<"$key_file")" != "$current_key" ]]; then
    mkdir -p "$cache_dir"
    "$forge_bin" zsh plugin > "$plugin_cache" 2>/dev/null || return 1
    printf '%s\n' "$current_key" > "$key_file"
  fi

  source "$plugin_cache"
  typeset -g _FORGE_PLUGIN_LOADED=1
}
_forge_plugin_init

# ---- section 2: per-shell rprompt cache ----
# Generate a unique ID for this shell session. PID + microsecond timestamp is
# collision-free on a single PID namespace.
# CONTAINER NOTE: this line must be revisited when container support lands —
# PID namespaces alias host PIDs and EPOCHREALTIME shares wall-clock across
# bind-mounted cache dirs, so collisions are possible between a host shell
# and a container shell forked at the same microsecond. See the file header.
typeset -g _FORGE_SHELL_ID="${_FORGE_SHELL_ID:-$$-${EPOCHREALTIME//./}}"
# CONTAINER NOTE: this cache path is shared host/container when ~/.cache is
# bind-mounted. Revisit alongside _FORGE_SHELL_ID when container support lands.
typeset -g _FORGE_RPROMPT_CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/forgecode/rprompt.${_FORGE_SHELL_ID}.txt"
typeset -gi _FORGE_RPROMPT_TTL=2

# Startup sweep: remove any rprompt cache files older than 24h. Protects
# against cache-dir bloat if shells die without running zshexit hooks. Runs
# in a detached subshell so it never blocks shell startup.
{
  local _forge_sweep_dir="${XDG_CACHE_HOME:-$HOME/.cache}/forgecode"
  [[ -d "$_forge_sweep_dir" ]] && find "$_forge_sweep_dir" -maxdepth 1 -name 'rprompt.*.txt' -mtime +1 -delete 2>/dev/null
} &!

# Async refresh on every precmd if the cache is stale. Non-blocking — the
# background subshell detaches and writes atomically via mv.
_forge_rprompt_refresh_if_stale() {
  command -v "${FORGE_BIN:-forge}" >/dev/null 2>&1 || return 0

  [[ -d "${_FORGE_RPROMPT_CACHE:h}" ]] || mkdir -p "${_FORGE_RPROMPT_CACHE:h}"

  if [[ -f "$_FORGE_RPROMPT_CACHE" ]]; then
    local -i age mtime
    mtime=$(zstat +mtime "$_FORGE_RPROMPT_CACHE" 2>/dev/null || echo 0)
    age=$(( EPOCHSECONDS - mtime ))
    (( age < _FORGE_RPROMPT_TTL )) && return 0
  fi

  {
    local tmp="${_FORGE_RPROMPT_CACHE}.tmp"
    _FORGE_ACTIVE_AGENT=$_FORGE_ACTIVE_AGENT \
    _FORGE_CONVERSATION_ID=$_FORGE_CONVERSATION_ID \
      "${FORGE_BIN:-forge}" zsh rprompt > "$tmp" 2>/dev/null \
      && mv -f "$tmp" "$_FORGE_RPROMPT_CACHE" \
      || rm -f "$tmp"
  } &!
}

autoload -Uz add-zsh-hook
add-zsh-hook precmd _forge_rprompt_refresh_if_stale

# On shell exit, clean up our per-shell cache file.
_forge_rprompt_cleanup() {
  [[ -f "$_FORGE_RPROMPT_CACHE" ]] && rm -f "$_FORGE_RPROMPT_CACHE"
}
add-zsh-hook zshexit _forge_rprompt_cleanup

# ---- section 3: p10k custom segment ----
# Reads the per-shell cache (sync, zero-cost) and emits it to p10k. forge's
# output already contains zsh color escapes so we pass it through verbatim.
#
# Returns silently if the cache is missing, empty, or unreadable — this makes
# the very first prompt (before any forge interaction has populated the cache)
# invisible rather than showing a broken segment.
function prompt_forge() {
  [[ -r "$_FORGE_RPROMPT_CACHE" && -s "$_FORGE_RPROMPT_CACHE" ]] || return 0
  local content
  content="$(<"$_FORGE_RPROMPT_CACHE")" 2>/dev/null || return 0
  [[ -z "$content" ]] && return 0
  p10k segment -t "${content}%f"
}

# p10k runs instant_prompt_* functions during instant-prompt replay. With
# POWERLEVEL9K_INSTANT_PROMPT=verbose (see p10k.zsh), any custom segment
# without a matching instant_prompt_* function prints a warning on every new
# shell. prompt_forge is a pure synchronous file read with no external state,
# so it is safe to call during instant-prompt replay.
function instant_prompt_forge() {
  prompt_forge
}

# Needed for p10k instant prompt compatibility — declares the segment has no
# dynamic classes, which allows p10k to cache the rendered output.
typeset -g POWERLEVEL9K_FORGE_CLASSES=()
