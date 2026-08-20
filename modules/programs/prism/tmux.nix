{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
let
  background = if type == "dark" then bg0 else bg_dim;
  isDarwin = pkgs.stdenv.hostPlatform.isDarwin;
  prismPkg = pkgs.callPackage ../../../pkgs/prism.nix { };
  prism = "${prismPkg}/bin/prism";

  # Sandboxed-pane guard for tmux if-shell.
  #
  # Returns 0 when the pane's current command indicates a prism agent pane
  # running under a supported isolation mode, 1 otherwise. The five
  # container-mode keybinds below (C-v paste bridge + C-u/C-d/C-g/C-M-g
  # agent scrolling) call this script via if-shell to decide whether to
  # fire the agent-specific action or fall through to a pass-through.
  #
  # The single argument is the value of tmux's #{pane_current_command} format
  # variable, expanded by tmux before the shell receives it.
  #
  # Matched values:
  #   bwrap    — bwrap isolation: `prism agent-run` execs bwrap directly, so
  #              the pane's direct child is the bwrap process.
  #   agent — defensive fallback for bwrap cases where tmux reports the
  #              foregrounded descendant rather than bwrap itself, and for
  #              host-mode panes that run pi directly (Linux legacy /
  #              Darwin). The agent (pi) is only ever launched in agent panes by
  #              prism, so matching it here does not affect plain shells,
  #              editors, or dashboard panes.
  #
  # Extracting this guard into a script keeps the five call sites consistent —
  # no binding keeps a hand-rolled pane_current_command equality check.
  sandboxedPaneGuard = pkgs.writeShellScript "prism-tmux-sandboxed-pane" ''
    case "$1" in
      bwrap) exit 0 ;;
      *) exit 1 ;;
    esac
  '';

  # Yank filter: strip the single leading U+0020 (space) that pi's card
  # rendering paints into every terminal buffer cell, but only when the
  # entire selection looks like a pi card (every non-empty line begins with
  # a space). When any non-empty line starts with a non-space character the
  # whole selection is passed through verbatim so indented code, ls -la
  # output, and other left-aligned content is never modified.
  #
  # Only U+0020 is handled. U+0009 (tab), U+00A0 (non-breaking space), and
  # box-drawing characters are treated as content and force the no-strip
  # branch. This is deliberate — revisit if pi ever uses a Unicode border
  # glyph for its card left edge.
  yankStripAwk = pkgs.writeText "prism-tmux-yank-strip.awk" ''
    {
      lines[NR] = $0
      if ($0 != "" && substr($0, 1, 1) != " ") any_no_lead = 1
    }
    END {
      for (i = 1; i <= NR; i++) {
        if (any_no_lead || substr(lines[i], 1, 1) != " ") print lines[i]
        else print substr(lines[i], 2)
      }
    }
  '';
  yankStrip = pkgs.writeShellScript "prism-tmux-yank-strip" ''
    exec ${pkgs.gawk}/bin/awk -f ${yankStripAwk}
  '';

  # Host-side clipboard paste bridge script for sandboxed agent panes.
  # Invoked by the tmux Ctrl-V keybind when the pane is running a prism
  # agent (see sandboxedPaneGuard above for the matching rules).
  # Argument $1 is the tmux pane ID (e.g. "%3") to inject the paste into.
  #
  # Two cases:
  #   1. Clipboard has a PNG: stage it and inject the path as bracketed-paste.
  #   2. Clipboard has text (or paste-image fails): read text from clipboard
  #      and inject it as bracketed-paste (preserving pre-PR text-paste behaviour).
  #
  # Using writeShellScript avoids complex quoting in the tmux bind-key string.
  # ESC bytes are written as literal escape chars inside the Nix string with $'\033'.
  clipboardPasteScript = pkgs.writeShellScript "prism-clipboard-paste" ''
    pane_id="$1"
    tmux="${pkgs.tmux}/bin/tmux"

    # Try image paste first.
    img="$(${prism} clipboard paste-image 2>/dev/null)"
    if [ -n "$img" ]; then
      # Image in clipboard: inject file path as bracketed-paste.
      # pi's drag-drop handler will stat() the path and attach the image.
      seq="$(printf '\033[200~%s\033[201~' "$img")"
      "$tmux" send-keys -t "$pane_id" -l -- "$seq"
      exit 0
    fi

    # No image: fall back to text clipboard paste.
    # This preserves pre-PR behaviour: before this keybind existed, the terminal
    # emulator's own bracketed-paste handled text Ctrl-V transparently through
    # the sandbox PTY (bwrap). Since we now intercept C-v
    # we must replicate this.
    if [ -n "$WAYLAND_DISPLAY" ]; then
      txt="$(wl-paste --no-newline 2>/dev/null)"
    elif [ -n "$DISPLAY" ]; then
      txt="$(xclip -selection clipboard -o 2>/dev/null)"
    fi
    if [ -n "$txt" ]; then
      seq="$(printf '\033[200~%s\033[201~' "$txt")"
      "$tmux" send-keys -t "$pane_id" -l -- "$seq"
    fi
  '';
in
{
  options = {
    nx.programs.prism.tmux.enable = lib.mkEnableOption "enables tmux" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.prism.tmux.enable {
    home-manager.users.${config.nx.username} =
      {
        lib,
        pkgs,
        ...
      }:
      {
        # Reload tmux config automatically after each nixos-rebuild switch.
        # Only runs if a tmux server is already running (exits silently otherwise).
        home.activation.reloadTmuxConfig = lib.hm.dag.entryAfter [ "writeBoundary" ] ''
          if ${pkgs.tmux}/bin/tmux list-sessions &>/dev/null 2>&1; then
            ${pkgs.tmux}/bin/tmux source-file ''${XDG_CONFIG_HOME:-$HOME/.config}/tmux/tmux.conf
          fi
        '';
        programs.tmux = {
          enable = true;
          secureSocket = false; # for some reason, tmux started via hyprland doesnt respect this and I only want 1 tmux server running
          aggressiveResize = true;
          escapeTime = 10; # no delay for escape key, vim style
          prefix = "C-a";
          terminal = if isDarwin then "xterm-kitty" else "kitty";
          extraConfig =
            # tmux
            ''
              # appearance
              set -g extended-keys on
              set -g extended-keys-format csi-u
              set -g status-interval 5
              set -g status-left-length 30
              set -g status-left " [#{session_name}] "
              # set -g status-right "#{?window_bigger,[#{window_offset_x}#,#{window_offset_y}] ,}#{=21:pane_title} "
              # status-right composition: aggregate waiting count first, then
              # the per-session status segment for the pane's session (currently
              # the mute indicator). Each helper emits "" in the empty case so
              # the visible bar collapses cleanly when nothing needs surfacing.
              # PRISM_SESSION_NAME is set on prism panes and read by the
              # session-status helper to know which session to render.
              set -g status-right "#(${prism} sessions status --waiting --tmux-format)#(PRISM_SESSION_NAME=#{session_name} ${prism} sessions session-status --tmux-format)"
              set -g status-style 'bg=${bg1} fg=${secondary}'
              set -g message-style 'bg=${primary} fg=${bg1}'
              set -g mode-style 'bg=${bg3} fg=${foreground}'
              set -g status-left-style 'bg=${bg1} fg=${secondary}'
              set -g status-right-style 'bg=${bg1} fg=${primary}'
              # for kitty images in image.nvim
              set -gq allow-passthrough on
              # sensible debugging behavior
              set -g remain-on-exit on
              # Enable OSC 52 clipboard passthrough so agent running inside a
              # sandbox (bwrap) can write to the host clipboard via the
              # sandbox PTY bridge. Without this, tmux drops OSC 52 sequences and
              # clipboard is silently broken.
              set -g set-clipboard on
              # pane switching
              bind -r h select-pane -L
              bind -r j select-pane -D
              bind -r k select-pane -U
              bind -r l select-pane -R
              # pane resizing
              bind -r ^h resize-pane -L 5
              bind -r ^j resize-pane -D 5
              bind -r ^k resize-pane -U 5
              bind -r ^l resize-pane -R 5
              # new window
              bind -r c new-window
              # window switching
              bind -r p previous-window
              bind -r n next-window
              # window splitting / detach — unbound (used by accident more than intentionally)
              unbind v
              unbind b
              unbind d
              # open current repo in browser (prefix+b)
              bind-key b run-shell -b '\
                dir="$(${pkgs.tmux}/bin/tmux display-message -p "#{pane_current_path}")"; \
                bare=""; \
                p="$dir"; \
                while [ "$p" != "/" ]; do \
                  if [ -d "$p/.bare" ]; then bare="$p/.bare"; break; fi; \
                  p="$(dirname "$p")"; \
                done; \
                if [ -n "$bare" ]; then \
                  url="$(git --git-dir="$bare" remote get-url origin 2>/dev/null)"; \
                else \
                  url="$(git -C "$dir" remote get-url origin 2>/dev/null)"; \
                fi; \
                if [ -z "$url" ]; then exit 0; fi; \
                url="$(echo "$url" | sed \
                  -e "s|git@\([^:]*\):\(.*\)\.git|https://\1/\2|" \
                  -e "s|git@\([^:]*\):\(.*\)|https://\1/\2|" \
                  -e "s|\.git$$||")"; \
                ${if isDarwin then "open" else "${pkgs.xdg-utils}/bin/xdg-open"} "$url" 2>/dev/null'

              # close window without confirmation
              bind-key X kill-window
              # close pane without confirmation
              bind-key x kill-pane
              # smart-decide session close (project@worktree sessions only):
              # - open PR        → soft close (preserve worktree + branch)
              # - merged/no PR   → hard cleanup (remove worktree + branch)
              # - probe failure  → fail-safe to soft close
              # See `prism close --help` for the full decision tree (issue #2179).
              bind-key q display-popup -E -w 60% -h 40% -b single "${prism} close --yes"
              # restart prism (prefix+R)
              bind-key R run-shell '${prism} restart'
              # toggle session mute (prefix+m). Operator escape hatch (see #2013):
              # silences this session's outbound coordinator notifications until
              # toggled off. The tmux session name doubles as the prism session
              # name on prism-managed panes. The display-message fallback covers
              # plain shell / dashboard panes (where the tmux session name does
              # not correspond to a prism session) so a stray Prefix+m there
              # does not crash tmux or surface a stack trace.
              bind-key m if-shell '[ -n "#{session_name}" ]' \
                'run-shell -b "if ${prism} mute #{session_name} >/dev/null; then ${pkgs.tmux}/bin/tmux refresh-client -S; else ${pkgs.tmux}/bin/tmux display-message \"prism mute: not a prism session\"; fi"' \
                'display-message "prism mute: no current session"'
              # easy config reload
              bind-key r source-file ~/.config/tmux/tmux.conf \; display-message "tmux.conf reloaded"

              # --- Prism-specific keybindings ---

              # context switcher popup (C-f)
              bind -n C-f display-popup -E -w 80% -h 80% -b single "${prism} switch"

              # C-w: run a fresh dashboard process directly in a popup.
              # Passes --caller-session so the "you are here" indicator works
              # correctly. The popup runs inside the caller's own client, so no
              # --caller-client flag is needed — no global tmux options written.
              # q/esc closes the popup via -E; no session involved.
              bind -n C-w display-popup -E -w 80% -h 60% -b single \
                "${prism} dashboard --popup --caller-session \"$(${pkgs.tmux}/bin/tmux display-message -p '#S')\""
              # prefix+D: switch to the persistent prism-dashboard session.
              # The session runs `prism dashboard` directly — no restart loop.
              # q/esc in that session uses switch-client -l to return the client
              # to wherever it came from (no global options needed).
              bind-key D run-shell \
                'if [ "$(${pkgs.tmux}/bin/tmux display-message -p "#S")" = "prism-dashboard" ]; then ${pkgs.tmux}/bin/tmux switch-client -l; else ${pkgs.tmux}/bin/tmux has-session -t prism-dashboard 2>/dev/null || ${pkgs.tmux}/bin/tmux new-session -ds prism-dashboard -n dashboard "${prism} dashboard"; ${pkgs.tmux}/bin/tmux switch-client -t prism-dashboard; fi'

              # toggle to/from term window (C-Space)
              unbind C-Space
              bind -n C-Space run-shell 'idx=$(${pkgs.tmux}/bin/tmux list-windows -F "##I:##W" | grep ":term$" | head -1 | cut -d: -f1); cur=$(${pkgs.tmux}/bin/tmux display-message -p "#{window_name}"); if [ -z "$idx" ]; then ${pkgs.tmux}/bin/tmux new-window -n term; elif [ "$cur" = "term" ]; then ${pkgs.tmux}/bin/tmux select-window -t agent; else ${pkgs.tmux}/bin/tmux select-window -t "$idx"; fi'

              # toggle to/from edit window (C-e)
              unbind C-e
              bind -n C-e run-shell 'idx=$(${pkgs.tmux}/bin/tmux list-windows -F "##I:##W" | grep ":edit$" | head -1 | cut -d: -f1); cur=$(${pkgs.tmux}/bin/tmux display-message -p "#{window_name}"); if [ -z "$idx" ]; then ${pkgs.tmux}/bin/tmux new-window -n edit; elif [ "$cur" = "edit" ]; then ${pkgs.tmux}/bin/tmux select-window -t agent; else ${pkgs.tmux}/bin/tmux select-window -t "$idx"; fi'

              # spawn new timestamped worktree from current repo (prefix+a)
              # prism spawn infers the repo from the current pane path, creates
              # a zettelkasten-timestamped branch+worktree, and switches to it.
              #
              # Wrapped in run-shell so we can resolve #{pane_current_path}
              # via `tmux display-message -p` at trigger time and substitute
              # the resolved string into `-d`. On tmux 3.6a,
              # `display-popup -e VAR=#{format}` does NOT run the value
              # through tmux format-expansion — a format like
              # `#{pane_current_path}` arrives inside the popup as the
              # literal string, not the resolved path (issue #2072). The
              # run-shell pattern resolves the path in the shell and feeds
              # it to `-d` directly, sidestepping the bug entirely. The
              # popup's cwd is enough context for `prism spawn` — its
              # `resolveBareRoot` falls back to `tmux.CurrentPanePath()`
              # for the bare-repo lookup, no env transport needed.
              #
              # PRISM_KEYBIND_SPAWN=1 is the dedicated keybind discriminator
              # (issue #2073). The empty-prompt guard (issue #1891) is
              # relaxed for the keybind path — the operator types the
              # initial prompt to the live agent after the popup attaches,
              # so requiring --prompt at invocation time would flash-close
              # the popup with an unreadable error (issue #2012). Using a
              # dedicated sentinel (rather than reusing PRISM_SPAWN_PATH)
              # ensures sandbox-injected env vars cannot accidentally trip
              # the carve-out from inside a container.
              bind a run-shell 'path=$(${pkgs.tmux}/bin/tmux display-message -p "#{pane_current_path}"); ${pkgs.tmux}/bin/tmux display-popup -E -d "$path" -w 60% -h 20% -b single -e "PRISM_KEYBIND_SPAWN=1" "${prism} spawn --attach"'

              # agent scrolling keybinds — active when the pane is a prism agent
              # running under bwrap isolation,
              # or hosting pi directly. See sandboxedPaneGuard above for the
              # matching rules. In any other pane (plain shell, editor, dashboard)
              # the literal keystroke is sent through unchanged.
              bind -n C-u if-shell '${sandboxedPaneGuard} "#{pane_current_command}"' 'send-keys C-M-u' 'send-keys C-u'
              bind -n C-d if-shell '${sandboxedPaneGuard} "#{pane_current_command}"' 'send-keys C-M-d' 'send-keys C-d'
              bind -n C-g if-shell '${sandboxedPaneGuard} "#{pane_current_command}"' 'send-keys Home'
              bind -n C-M-g if-shell '${sandboxedPaneGuard} "#{pane_current_command}"' 'send-keys End'

              # Directional session switching for the agent window (issue #1794).
              #
              # Root-level C-h/C-j/C-k/C-l invoke `prism nav <dir>` when the active
              # window is named `agent`; on every other window (edit / term /
              # custom) the literal keystroke is sent through unchanged via
              # `send-keys`. This preserves neovim's window navigation on the
              # `edit` window and the shell's C-h (backspace) on the `term` window.
              #
              # The guard is window-name based ("#{window_name}" = "agent"), not
              # pane-command based, because the agent window may be on its second
              # pane (a shell next to pi) and the keystroke should still navigate
              # sessions in that case.
              #
              # `run-shell -b` runs `prism nav` in the background so the tmux
              # binding returns immediately; the nav subcommand calls
              # `tmux switch-client` itself.
              bind -n C-h if-shell '[ "#{window_name}" = "agent" ]' \
                'run-shell -b "${prism} nav left"' \
                'send-keys C-h'
              bind -n C-j if-shell '[ "#{window_name}" = "agent" ]' \
                'run-shell -b "${prism} nav down"' \
                'send-keys C-j'
              bind -n C-k if-shell '[ "#{window_name}" = "agent" ]' \
                'run-shell -b "${prism} nav up"' \
                'send-keys C-k'
              bind -n C-l if-shell '[ "#{window_name}" = "agent" ]' \
                'run-shell -b "${prism} nav right"' \
                'send-keys C-l'

              # Clipboard paste bridge for sandboxed agent panes (issue #752).
              #
              # When Ctrl-V is pressed in a pane whose current command is one of
              # the sandboxed-agent values matched by sandboxedPaneGuard
              # (bwrap / direct pi):
              #
              #   Image path: `prism clipboard paste-image` reads the host clipboard,
              #   stages the PNG to ~/.cache/prism/clipboard/<uuid>.png, and prints
              #   the path. The path is injected into the pane as a bracketed-paste
              #   sequence so pi's existing drag-drop handler attaches it.
              #
              #   Text fallback path: if paste-image exits non-zero (no image in
              #   clipboard), read text from the host clipboard and inject it as a
              #   bracketed-paste sequence. This ensures text Ctrl-V continues to
              #   work in agent panes even though tmux has intercepted C-v. Without
              #   this fallback, text paste would be silently dropped.
              #
              # Guard: if-shell runs sandboxedPaneGuard against #{pane_current_command}
              # and only intercepts when the pane is a sandboxed agent pane. All
              # other panes (plain shell, editor, non-agent windows) are unaffected —
              # Ctrl-V reaches them via the else branch which sends a literal Ctrl-V
              # (standard bracketed-paste passthrough).
              #
              # The script is written to the Nix store via writeShellScript to avoid
              # complex quoting of ESC bytes and printf format strings inside the
              # tmux bind-key string. #{pane_id} is expanded by tmux before the
              # shell receives it as $1.
              bind -n C-v \
                if-shell '${sandboxedPaneGuard} "#{pane_current_command}"' \
                  'run-shell "${clipboardPasteScript} #{pane_id}"' \
                  'send-keys C-v'

              # toggle a split pane with edit and agent
              bind-key Space if-shell "[ $(tmux display-message -t 'edit' -p '#{window_panes}') -gt 1 ]" \
                  "break-pane -s 'edit.1' -n 'agent'" \
                  "join-pane -h -s 'agent.0' -t 'edit'"

              # --- DB lifecycle hooks ---

              # session-created hook intentionally omitted: the explicit call in
              # ensureAndSwitchSession (switch.go) is the authoritative seed for
              # agent_status. The hook's #{pane_current_path} races the new
              # session's first pane path and permanently corrupts the worktree
              # field when invoked from a display-popup. See issue #380.

              # Write tmux_session_end event when a session is closed.
              set-hook -g session-closed "run-shell '${prism} event tmux-session-end --session #{session_name}'"
              # Transition to interrupted when a pane dies unexpectedly mid-session.
              # Only the agent window exit is relevant; prism event pane-died
              # ignores exits from other windows (term, edit) and is a no-op for
              # non-project sessions and sessions already in a terminal state.
              # #{pane_dead_status} is passed so pane-died can override a prior
              # "finished" state when the exit was non-zero (signal/crash).
              set-hook -g pane-exited "run-shell '${prism} event pane-died --session #{session_name} --window #{window_name} --exit-code #{pane_dead_status}'"

              # Remove HM session vars guard from tmux environment so new shells
              # re-evaluate $(cat ...) substitutions for secrets like GITHUB_TOKEN
              set-environment -gr __HM_SESS_VARS_SOURCED

              # vim style copy
              set -g mode-keys vi
              bind-key -T copy-mode-vi 'v' send -X begin-selection
              bind-key -T copy-mode-vi 'V' send -X select-line
              bind-key -T copy-mode-vi 'y' send -X copy-pipe-and-cancel "${yankStrip}"
              bind-key -T copy-mode-vi 'q' send -X cancel
              bind-key -T copy-mode-vi Escape send -X cancel
            '';
        };

        # Restore tmux sessions once after login.
        # server-started fires before the config is sourced, so the hook
        # registered in extraConfig is always too late. A systemd user service
        # is the reliable alternative: it runs after graphical-session.target,
        # at which point tmux will be started (or already running), and
        # prism restore recreates any sessions not yet present.
        #
        # ExecStart is wrapped in `systemd-run --user --scope --collect` so
        # the tmux server (auto-started by restore's tmux calls) and the
        # sidecar processes spawned by `session.StartSidecarWithOpts` land
        # in a transient scope unit rather than in this service's cgroup.
        # Without the wrapper, this Type=oneshot unit's default
        # KillMode=control-group SIGTERMs every process restore just
        # created the moment the main process exits — see issue #2340
        # for the full diagnosis. The scope's lifetime is bound to its
        # member processes (not to this service), so the restored tmux
        # server and sidecars survive service deactivation and any later
        # stop/restart (e.g. an HM reload during `nh switch`). The scope
        # name is auto-generated (run-uN.scope) so repeated logins never
        # collide; --collect unloads the transient unit once its last
        # process exits, so the no-sessions-to-restore case leaves no
        # stray units accumulating across logins. --quiet suppresses the
        # "Running as unit: …" info line in the journal.
        systemd.user.services.prism-restore = {
          Unit = {
            Description = "Restore prism tmux sessions after login";
            After = [
              "graphical-session.target"
            ];
          };
          Service = {
            Type = "oneshot";
            # Give the desktop session a moment to settle before poking tmux.
            ExecStartPre = "${pkgs.coreutils}/bin/sleep 2";
            ExecStart = "${pkgs.systemd}/bin/systemd-run --user --scope --collect --quiet ${prismPkg}/bin/prism restore";
          };
          Install = {
            WantedBy = [ "graphical-session.target" ];
          };
        };

        home.persistence."/persist" = {
          directories = [
            ".local/share/prism"
            ".local/state/prism"
          ];
        };
      };
  };
}
