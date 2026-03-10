{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.chromium.enable = lib.mkEnableOption "enables chromium" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.chromium.enable {
    home-manager.users.${config.nx.username} = {
      programs.chromium = {
        enable = true;
        package = pkgs.chromium;
        commandLineArgs = [
          "--enable-features=UseOzonePlatform"
          "--ozone-platform=wayland"
        ];
      };
      xdg.desktopEntries.chromium-browser = lib.mkIf pkgs.stdenv.isLinux {
        name = "Chromium";
        genericName = "Web Browser";
        exec = "chromium-browser %U";
        icon = "chromium";
        type = "Application";
        categories = [
          "Network"
          "WebBrowser"
        ];
        mimeType = [
          "text/html"
          "text/xml"
          "application/xhtml+xml"
          "x-scheme-handler/http"
          "x-scheme-handler/https"
        ];
      };
      wayland.windowManager.hyprland.settings =
        lib.mkIf (config.home-manager.users.${config.nx.username}.wayland.windowManager.hyprland.enable)
          {
            windowrule = [
              # sometimes chromium thinks its fine to open in a tiny window
              "tile on, match:class Chromium-browser"
            ];
          };
    };
  };
}
