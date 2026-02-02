{
  config,
  pkgs,
  lib,
  ...
}:
with config.theme;
{
  config = lib.mkIf (config.nx.desktop.rofi.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben.home.file.".local/scripts/application.nvim.sessionLauncher" =
      lib.mkIf config.nx.programs.prism.sessioniser.enable
        {
          executable = true;
          text =
            let
              nvimSessionLauncherStyle = ''inputbar { children: [entry]; border-color: ${blue};} entry { placeholder: "Select Project"; } element-icon { enabled: false; }'';
            in
            # bash
            ''
              #!/bin/sh
              if [ "$XDG_SESSION_DESKTOP" = sway ]; then
                monitor="$(swaymsg -t get_outputs | jq -c '.[] | select(.focused) | select(.id)' | jq -c '.name')"
              elif [ "$XDG_SESSION_DESKTOP" = Hyprland ]; then
                monitor="$(hyprctl -j monitors | jq -c '.[] | select(.focused == true)' | jq -r '.name')"
              else
                monitor="DP-3"
              fi
              # Get the selection from tmux project getter
              selection=$(~/.local/scripts/cli.tmux.projectGetter | rofi -monitor "$monitor" -dmenu -theme-str '${nvimSessionLauncherStyle}')
              if [ -n "$selection" ]; then
                # Run the tmux sessioniser with the selected session
                xargs -I{} ${pkgs.kitty}/bin/kitty ~/.local/scripts/cli.tmux.projectSessioniser "{}" 2> /dev/null <<<"$selection"
              fi
            '';
        };
  };
}
