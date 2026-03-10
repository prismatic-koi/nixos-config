{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
{
  config = lib.mkIf (config.nx.desktop.rofi.enable && pkgs.stdenv.isLinux) {
    home-manager.users.${config.nx.username}.home = {
      file.".local/scripts/application.rofi.emojipicker" = {
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
            rofi_style_emojipicker='inputbar { children: [entry]; border-color: ${blue};} entry { placeholder: "Select Emoji"; } element-icon { enabled: false; }'

            # emoji list with a bunch of things taken out to make it more manageable
            custom_emoji_list="/home/ben/.config/rofi-emoji/custom_emoji_list.txt"

            # start rofi with emoji args
            rofi -monitor "$monitor" -show emoji -emoji-file "$custom_emoji_list" -theme-str "$rofi_style_emojipicker"
          '';
      };
      # a custom emoji list for rofi-emoji
      # I want to filter out skintone, gendered emoji and country flags that I don't use
      file.".config/rofi-emoji/custom_emoji_list.txt" = {
        source =
          let
            emojiRaw = builtins.fetchurl {
              url = "https://raw.githubusercontent.com/Mange/rofi-emoji/refs/heads/master/all_emojis.txt";
              sha256 = "1grr19rg5a8xl6mjnjc1fvmf7zx9jfj6y33qcasd2j1cvx8k5lbd";
            };
          in
          pkgs.runCommand "filtered-emojis" { nativeBuildInputs = [ pkgs.ripgrep ]; } ''
            cat ${emojiRaw} | ${pkgs.ripgrep}/bin/rg -v '([🏻-🏿♀♂⚧👩👨]|country-flag|subdivision-flag)' > $out
            if [ $? -ne 0 ]; then
              echo "ripgrep failed!" >&2
              exit 1
            fi
          '';
      };
    };
  };
}
