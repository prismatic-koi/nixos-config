{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.gaming.steam.enable = lib.mkEnableOption "enables steam" // {
      default = true;
    };
    nx.gaming.steam.persist = lib.mkEnableOption "enables persistence for steam directory" // {
      default = true;
    };
  };
  config =
    lib.mkIf
      (
        config.nx.gaming.steam.enable
        # only enable if gaming is enabled
        && config.nx.gaming.enable
        && pkgs.stdenv.isLinux
      )
      {
        programs.steam = {
          enable = true;
          gamescopeSession.enable = false;
          protontricks.enable = true;
          package = pkgs.steam.override {
            extraPkgs =
              pkgs: with pkgs; [
                libcap
                procps
                usbutils
              ];
          };
        };
        environment.sessionVariables = {
          STEAM_EXTRA_COMPAT_TOOLS_PATHS = "/home/ben/.steam/root/compatibilitytools.d";
        };
        home-manager.users.${config.nx.username} = {
          home.persistence."/persist" = {
            directories = [
              (lib.mkIf config.nx.gaming.steam.persist ".local/share/Steam")
              ".local/share/applications" # where steam puts its .desktop files for games
              ".local/share/icons/hicolor" # where steam puts its icons
            ];
          };
          wayland.windowManager.hyprland.settings =
            lib.mkIf (config.home-manager.users.${config.nx.username}.wayland.windowManager.hyprland.enable)
              {
                windowrule = [
                  # fake fullscreen, good store videos
                  "sync_fullscreen 0, match:class steam"
                ];
              };
        };
      };
}
