{
  config,
  pkgs,
  lib,
  ...
}:
{
  config = lib.mkIf (config.nx.desktop.sway.enable && pkgs.stdenv.isLinux) {
    home-manager.users.${config.nx.username}.wayland.windowManager.sway.config = {
      startup = [
        {
          # swayidle: lock screen after 30 minutes of inactivity
          # turn off screen after 1 hour of inactivity
          command = ''
            exec ${pkgs.swayidle}/bin/swayidle -w \
              timeout 1800 '${pkgs.swaylock}/bin/swaylock -f -c 000000' \
              timeout 3600 'swaymsg "output * power off"' resume 'swaymsg "output * power on"' \
              before-sleep '${pkgs.swaylock}/bin/swaylock -f -c 000000'
          '';
        }
      ];
    };
  };
}
