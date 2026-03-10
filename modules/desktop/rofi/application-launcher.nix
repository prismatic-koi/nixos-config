{
  config,
  pkgs,
  lib,
  ...
}:
{
  config = lib.mkIf (config.nx.desktop.rofi.enable && pkgs.stdenv.isLinux) {
    home-manager.users.${config.nx.username}.home.file.".local/scripts/application.launcher" = {
      executable = true;
      text =
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

          # style for app picker
          rofi_style_dmenu='entry { placeholder: "drun"; }'

          rofi -monitor "$monitor" -show drun -theme-str "$rofi_style_dmenu"
        '';
    };
  };
}
