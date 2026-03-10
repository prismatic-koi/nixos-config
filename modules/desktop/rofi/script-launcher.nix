{
  config,
  pkgs,
  lib,
  ...
}:
{
  config = lib.mkIf (config.nx.desktop.rofi.enable && pkgs.stdenv.isLinux) {
    home-manager.users.${config.nx.username}.home.file.".local/scripts/application.scripts.launcher" = {
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
          rofi_style_runscripts='inputbar { children: [entry];} entry { placeholder: "Select Command"; }  element-icon { enabled: false; }'

          # get list of scripts in /home/ben/.local/scripts
          scripts=$(ls /home/ben/.local/scripts)
          # filter out any prefixed with cli, these arent meant to be run this way
          scripts=$(echo "$scripts" | grep -v '^cli.')

          # Pass the list to rofi and store the selected script in a variable
          selected_script=$(echo "$scripts" | rofi -monitor "$monitor" -dmenu -i -font "JetBrainsMono NF 14" -theme-str "$rofi_style_runscripts")
          # Execute the selected script
          if [[ -n $selected_script ]]; then
              bash "/home/ben/.local/scripts/$selected_script"
          fi
        '';
    };
  };
}
