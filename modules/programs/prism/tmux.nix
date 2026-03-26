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
in
{
  options = {
    nx.programs.prism.tmux.enable = lib.mkEnableOption "enables tmux" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.prism.tmux.enable {
    home-manager.users.${config.nx.username} = {
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
            set -g status-left-length 30
            set -g status-left " [#{session_name}] "
            # set -g status-right "#{?window_bigger,[#{window_offset_x}#,#{window_offset_y}] ,}#{=21:pane_title} "
            set -g status-right "#h "
            set -g status-style 'bg=${bg1} fg=${secondary}'
            set -g message-style 'bg=${primary} fg=${bg1}'
            set -g mode-style 'bg=${bg3} fg=${foreground}'
            set -g status-left-style 'bg=${bg1} fg=${secondary}'
            set -g status-right-style 'bg=${bg1} fg=${primary}'
            # for kitty images in image.nvim
            set -gq allow-passthrough on
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
            # window splitting — unbound (used by accident more than intentionally)
            unbind v
            unbind b
            # open current repo in browser (prefix+b)
            bind-key b run-shell '\
              dir="$(tmux display-message -p "#{pane_current_path}")"; \
              url="$(git -C "$dir" remote get-url origin 2>/dev/null)"; \
              if [ -z "$url" ]; then exit 0; fi; \
              url="$(echo "$url" | sed \
                -e "s|git@\([^:]*\):\(.*\)\.git|https://\1/\2|" \
                -e "s|git@\([^:]*\):\(.*\)|https://\1/\2|" \
                -e "s|\.git$$||")"; \
              ${if isDarwin then "open" else "${pkgs.xdg-utils}/bin/xdg-open"} "$url" 2>/dev/null &'

            # close window without confirmation
            bind-key X kill-window
            # close pane without confirmation
            bind-key x kill-pane
            # worktree cleanup: remove worktree + kill session (project@worktree sessions only)
            bind-key W display-popup -E -w 60% -h 40% -b single "prism cleanup"
            # easy config reload
            bind-key r source-file ~/.config/tmux/tmux.conf \; display-message "tmux.conf reloaded"

            # --- Prism-specific keybindings ---

            # context switcher popup (C-f)
            bind -n C-f display-popup -E -w 80% -h 80% -b single "prism switch"

            # C-w: run a fresh dashboard process directly in a popup.
            # Simple and reliable — q/esc closes the popup via -E, no session involved.
            # Still stamps @prism_caller/@prism_caller_client for the 'you are here'
            # indicator and Enter navigation.
            bind -n C-w run-shell \
              'tmux set-option -g @prism_caller "$(tmux display-message -p "#S")"; tmux set-option -g @prism_caller_client "$(tmux display-message -p "#{client_name}")"; tmux display-popup -E -w 80% -h 60% -b single "prism dashboard --popup"'
            # prefix+D: switch to the persistent prism-dashboard session.
            # q/esc in that session detaches the client back to previous session.
            bind-key D run-shell \
              'if [ "$(tmux display-message -p "#S")" = "prism-dashboard" ]; then tmux detach-client; else tmux set-option -g @prism_caller "$(tmux display-message -p "#S")"; tmux set-option -g @prism_caller_client "$(tmux display-message -p "#{client_name}")"; tmux has-session -t prism-dashboard 2>/dev/null || tmux new-session -ds prism-dashboard -n dashboard "while true; do prism dashboard --popup; done"; tmux switch-client -t prism-dashboard; fi'

            # toggle to/from term window (C-Space)
            unbind C-Space
            bind -n C-Space run-shell 'idx=$(${pkgs.tmux}/bin/tmux list-windows -F "##I:##W" | grep ":term$" | head -1 | cut -d: -f1); cur=$(${pkgs.tmux}/bin/tmux display-message -p "#{window_name}"); if [ -z "$idx" ]; then ${pkgs.tmux}/bin/tmux new-window -n term; elif [ "$cur" = "term" ]; then ${pkgs.tmux}/bin/tmux last-window; else ${pkgs.tmux}/bin/tmux select-window -t "$idx"; fi'

            # toggle to/from edit window (C-e)
            unbind C-e
            bind -n C-e run-shell 'idx=$(${pkgs.tmux}/bin/tmux list-windows -F "##I:##W" | grep ":edit$" | head -1 | cut -d: -f1); cur=$(${pkgs.tmux}/bin/tmux display-message -p "#{window_name}"); if [ -z "$idx" ]; then ${pkgs.tmux}/bin/tmux new-window -n edit; elif [ "$cur" = "edit" ]; then ${pkgs.tmux}/bin/tmux last-window; else ${pkgs.tmux}/bin/tmux select-window -t "$idx"; fi'

            # spawn new timestamped worktree from current repo (prefix+a)
            # prism spawn infers the repo from the current pane path, creates
            # a zettelkasten-timestamped branch+worktree, and switches to it.
            bind a run-shell \
              'tmux display-popup -E -w 60% -h 20% -b single "prism spawn || { echo; printf \"press any key to close\"; read -rn1; }"'

            # opencode scrolling keybinds (only active when opencode is running)
            # Note: on NixOS/Linux, opencode runs directly as "opencode" in pane_current_command
            bind -n C-u if-shell '[ "#{pane_current_command}" = "opencode" ]' 'send-keys C-M-u' 'send-keys C-u'
            bind -n C-d if-shell '[ "#{pane_current_command}" = "opencode" ]' 'send-keys C-M-d' 'send-keys C-d'
            bind -n C-g if-shell '[ "#{pane_current_command}" = "opencode" ]' 'send-keys Home'
            bind -n C-M-g if-shell '[ "#{pane_current_command}" = "opencode" ]' 'send-keys End'

            # toggle a split pane with edit and agent
            bind-key Space if-shell "[ $(tmux display-message -t 'edit' -p '#{window_panes}') -gt 1 ]" \
                "break-pane -s 'edit.1' -n 'agent'" \
                "join-pane -h -s 'agent.0' -t 'edit'"

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
    };
  };
}
