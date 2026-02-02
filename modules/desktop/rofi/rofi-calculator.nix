{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
{
  config = lib.mkIf (config.nx.desktop.rofi.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben.home.file.".local/scripts/application.rofi.calculator" = {
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

          # rofi style
          rofi_style_calculator='listview { enabled: false;} inputbar { children: [entry]; border-color: ${blue};} entry { placeholder: "Calculator"; } element-icon { enabled: false; }'

          # start rofi with calculator args
          rofi -monitor "$monitor" -show calc -no-history -calc-command 'wl-copy "{result}"' -theme-str "$rofi_style_calculator"
        '';
    };
  };
}
