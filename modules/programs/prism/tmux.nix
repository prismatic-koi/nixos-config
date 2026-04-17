{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
let
  background = if type == "dark" then bg0 else bg_dim;
  isDarwin = pkgs.stdenv.isDarwin;
  prismPkg = pkgs.callPackage ../../../pkgs/prism.nix { };
  prism = "${prismPkg}/bin/prism";

  # Host-side clipboard paste bridge script for container-mode opencode panes.
  # Invoked by the tmux Ctrl-V keybind when pane_current_command is "podman".
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
      # opencode's drag-drop handler will stat() the path and attach the image.
      seq="$(printf '\033[200~%s\033[201~' "$img")"
      "$tmux" send-keys -t "$pane_id" -l -- "$seq"
      exit 0
    fi

    # No image: fall back to text clipboard paste.
    # This preserves pre-PR behaviour: before this keybind existed, the terminal
    # emulator's own bracketed-paste handled text Ctrl-V transparently through
    # the podman attach PTY. Since we now intercept C-v we must replicate this.
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
              set -g status-right "#(${prism} status --waiting --tmux-format)#h "
              set -g status-style 'bg=${bg1} fg=${secondary}'
              set -g message-style 'bg=${primary} fg=${bg1}'
              set -g mode-style 'bg=${bg3} fg=${foreground}'
              set -g status-left-style 'bg=${bg1} fg=${secondary}'
              set -g status-right-style 'bg=${bg1} fg=${primary}'
              # for kitty images in image.nvim
              set -gq allow-passthrough on
              # Enable OSC 52 clipboard passthrough so opencode running inside a
              # container can write to the host clipboard via the podman attach PTY bridge.
              # Without this, tmux drops OSC 52 sequences and clipboard is silently broken.
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
              # worktree cleanup: remove worktree + kill session (project@worktree sessions only)
              bind-key q display-popup -E -w 60% -h 40% -b single "${prism} cleanup"
              # restart prism (prefix+R)
              bind-key R run-shell '${prism} restart'
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
              # -d sets the popup's start directory to the caller's pane path
              # (#{pane_current_path} is expanded by tmux for -d). The popup
              # pane then reports that same path via pane_current_path, so
              # prism's CurrentPanePath() fallback works without PRISM_SPAWN_PATH.
              bind a display-popup -E -d "#{pane_current_path}" -w 60% -h 20% -b single \
                "${prism} spawn --attach"

              # opencode scrolling keybinds (only active when opencode is running inside podman)
              # Note: after the podman-attach migration, the agent pane command is "podman"
              bind -n C-u if-shell '[ "#{pane_current_command}" = "podman" ]' 'send-keys C-M-u' 'send-keys C-u'
              bind -n C-d if-shell '[ "#{pane_current_command}" = "podman" ]' 'send-keys C-M-d' 'send-keys C-d'
              bind -n C-g if-shell '[ "#{pane_current_command}" = "podman" ]' 'send-keys Home'
              bind -n C-M-g if-shell '[ "#{pane_current_command}" = "podman" ]' 'send-keys End'

              # Clipboard paste bridge for container-mode opencode panes (issue #752).
              #
              # When Ctrl-V is pressed in a pane whose current command is "podman"
              # (i.e. `podman attach` is running the container's opencode TUI):
              #
              #   Image path: `prism clipboard paste-image` reads the host clipboard,
              #   stages the PNG to ~/.cache/prism/clipboard/<uuid>.png, and prints
              #   the path. The path is injected into the pane as a bracketed-paste
              #   sequence so opencode's existing drag-drop handler attaches it.
              #
              #   Text fallback path: if paste-image exits non-zero (no image in
              #   clipboard), read text from the host clipboard and inject it as a
              #   bracketed-paste sequence. This ensures text Ctrl-V continues to
              #   work in container panes even though tmux has intercepted C-v.
              #   Without this fallback, text paste would be silently dropped.
              #
              # Guard: if-shell checks pane_current_command and only intercepts when
              # it equals "podman". All other panes (plain shell, editor, non-container
              # windows) are unaffected — Ctrl-V reaches them via the else branch
              # which sends a literal Ctrl-V (standard bracketed-paste passthrough).
              #
              # The script is written to the Nix store via writeShellScript to avoid
              # complex quoting of ESC bytes and printf format strings inside the
              # tmux bind-key string. #{pane_id} is expanded by tmux before the
              # shell receives it as $1.
              bind -n C-v \
                if-shell '[ "#{pane_current_command}" = "podman" ]' \
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
              set-environment -r __HM_SESS_VARS_SOURCED

              # vim style copy
              set -g mode-keys vi
              bind-key -T copy-mode-vi 'v' send -X begin-selection
              bind-key -T copy-mode-vi 'V' send -X select-line
              bind-key -T copy-mode-vi 'y' send -X copy-selection-and-cancel
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
        systemd.user.services.prism-restore = {
          Unit = {
            Description = "Restore prism tmux sessions after login";
            After = [
              "graphical-session.target"
              # Ensure the container image is loaded before we try to spawn containers.
              "prism-agent-image.service"
            ];
            Wants = [ "prism-agent-image.service" ];
          };
          Service = {
            Type = "oneshot";
            # Give the desktop session a moment to settle before poking tmux.
            ExecStartPre = "${pkgs.coreutils}/bin/sleep 2";
            ExecStart = "${prismPkg}/bin/prism restore";
          };
          Install = {
            WantedBy = [ "graphical-session.target" ];
          };
        };

        home.persistence."/persist" = {
          directories = [
            ".local/state/prism"
          ];
        };
      };
  };
}
