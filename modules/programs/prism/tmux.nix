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
            # window splitting
            bind -r v split-window -v
            bind -r b split-window -h
            # close window without confirmation
            bind-key X kill-window
            # close pane without confirmation
            bind-key x kill-pane
            # worktree cleanup: remove worktree + kill session (project@worktree sessions only)
            # TMUX_PANE is inherited by the popup shell; the script uses it to resolve the session
            bind-key W display-popup -E -w 60% -h 40% -b single "cli.tmux.worktreeCleanup"
            # easy config reload
            bind-key r source-file ~/.config/tmux/tmux.conf \; display-message "tmux.conf reloaded"

            # --- Prism-specific keybindings ---

            # context switcher popup (C-f)
            bind -n C-f display-popup -E -w 80% -h 80% -b single "cli.tmux.contextSwitcher"

            # C-w: if already in prism-dashboard, detach back to last session.
            # Otherwise stamp the calling session into @prism_caller, ensure
            # the dashboard session exists, then attach to it via popup.
            bind -n C-w run-shell \
              'if [ "$(tmux display-message -p "#S")" = "prism-dashboard" ]; then tmux detach-client; else tmux set-option -g @prism_caller "$(tmux display-message -p "#S")"; tmux has-session -t prism-dashboard 2>/dev/null || tmux new-session -ds prism-dashboard -n dashboard "while true; do prism dashboard --popup; done"; tmux display-popup -E -w 80% -h 60% -b single "tmux attach-session -t prism-dashboard"; fi'
            # prefix+D: same but switch-client instead of popup.
            bind-key D run-shell \
              'if [ "$(tmux display-message -p "#S")" = "prism-dashboard" ]; then tmux detach-client; else tmux set-option -g @prism_caller "$(tmux display-message -p "#S")"; tmux has-session -t prism-dashboard 2>/dev/null || tmux new-session -ds prism-dashboard -n dashboard "while true; do prism dashboard --popup; done"; tmux switch-client -t prism-dashboard; fi'

            # toggle to/from term window (C-Space)
            unbind C-Space
            bind -n C-Space run-shell 'idx=$(${pkgs.tmux}/bin/tmux list-windows -F "##I:##W" | grep ":term$" | head -1 | cut -d: -f1); cur=$(${pkgs.tmux}/bin/tmux display-message -p "#{window_name}"); if [ -z "$idx" ]; then ${pkgs.tmux}/bin/tmux new-window -n term; elif [ "$cur" = "term" ]; then ${pkgs.tmux}/bin/tmux last-window; else ${pkgs.tmux}/bin/tmux select-window -t "$idx"; fi'

            # toggle to/from edit window (C-e)
            unbind C-e
            bind -n C-e run-shell 'idx=$(${pkgs.tmux}/bin/tmux list-windows -F "##I:##W" | grep ":edit$" | head -1 | cut -d: -f1); cur=$(${pkgs.tmux}/bin/tmux display-message -p "#{window_name}"); if [ -z "$idx" ]; then ${pkgs.tmux}/bin/tmux new-window -n edit; elif [ "$cur" = "edit" ]; then ${pkgs.tmux}/bin/tmux last-window; else ${pkgs.tmux}/bin/tmux select-window -t "$idx"; fi'

            # new window with opencode agent (prefix + a)
            bind a new-window -n agent "zsh -ic opencode"

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
