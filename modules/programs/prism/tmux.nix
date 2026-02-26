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
    home-manager.users.ben = {
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
            # easy config reload
            bind-key r source-file ~/.config/tmux/tmux.conf \; display-message "tmux.conf reloaded"

            # --- Prism-specific keybindings ---

            # context switcher popup (C-f)
            bind -n C-f display-popup -E -w 80% -h 80% -b single "cli.tmux.contextSwitcher"

            # toggle to/from term window (C-Space)
            unbind C-Space
            bind -n C-Space if-shell 'tmux list-windows -F "##I:##W" | grep -q ":term$"' \
              'if-shell "[ #{window_name} = term ]" "last-window" "select-window -t term"' \
              'new-window -n term'

            # toggle to/from edit window (C-e)
            unbind C-e
            bind -n C-e if-shell 'tmux list-windows -F "##I:##W" | grep -q ":edit$"' \
              'if-shell "[ #{window_name} = edit ]" "last-window" "select-window -t edit"' \
              'new-window -n edit'

            # new window with opencode agent (prefix + a)
            bind a new-window -n agent "opencode"

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
