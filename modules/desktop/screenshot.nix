{
  config,
  pkgs,
  lib,
  ...
}:
let
  theme = config.theme;
in
{
  options = {
    nx.desktop.screenshot.enable = lib.mkEnableOption "enables screenshot utils" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.desktop.screenshot.enable && pkgs.stdenv.isLinux) {
    home-manager.users.${config.nx.username} = {
      home = {
        file.".local/scripts/application.grim.screenshotToClipboard" = {
          executable = true;
          text = ''
            #!/bin/sh
            ${pkgs.grim}/bin/grim -g "$(${pkgs.slurp}/bin/slurp -c "${theme.green}FF" -b '${theme.bg0}80')" - | ${pkgs.wl-clipboard}/bin/wl-copy
          '';
        };
        file.".local/scripts/application.grim.screenshotToFile" = {
          executable = true;
          text = ''
            #!/bin/sh
            mkdir -p "$HOME/pictures/screenshots"
            ${pkgs.grim}/bin/grim -g "$(${pkgs.slurp}/bin/slurp -c "${theme.green}FF" -b '${theme.bg0}80')" "$HOME/pictures/screenshots/$(date '+%y%m%d_%H-%M-%S').png"
          '';
        };
        file.".local/scripts/application.grim.fullScreenshotToFile" = {
          executable = true;
          text = ''
            #!/bin/sh
            sleep 0.2 # wait for the ui to move if done from command palette
            mkdir -p "$HOME/pictures/screenshots"
            ${pkgs.grim}/bin/grim "$HOME/pictures/screenshots/$(date '+%y%m%d_%H-%M-%S').png"
          '';
        };
      };
      wayland.windowManager =
        let
          homeDir = config.home-manager.users.${config.nx.username}.home.homeDirectory;
          screenshotToClipboard = "${homeDir}/.local/scripts/application.grim.screenshotToClipboard";
        in
        {
          # shortcuts for sway
          sway.config.keybindings =
            let
              super = "Mod4";
            in
            {
              "${super}+Shift+s" = "exec ${screenshotToClipboard}";
            };
          # shortcuts for hyprland
          hyprland.settings.bind = [
            "SUPER SHIFT, S, exec, ${screenshotToClipboard}"
          ];
        };
    };
  };
}
