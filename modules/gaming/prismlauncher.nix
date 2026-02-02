{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.gaming.prismlauncher.enable =
      lib.mkEnableOption "enables prismlauncher, open source minecraft launcher"
      // {
        default = false;
      };
  };
  config =
    lib.mkIf
      (
        config.nx.gaming.prismlauncher.enable
        # only enable if gaming is enabled
        && config.nx.gaming.enable
        && pkgs.stdenv.isLinux
      )
      {
        home-manager.users.ben = {
          home.packages = with pkgs; [
            prismlauncher
          ];
          home.persistence."/persist" = {
            directories = [
              ".local/share/PrismLauncher"
            ];
          };
          wayland.windowManager.hyprland.settings =
            lib.mkIf (config.home-manager.users.ben.wayland.windowManager.hyprland.enable)
              {
                windowrule = [
                  "tile on, match:class PrismLauncher"
                ];
              };
        };
      };
}
