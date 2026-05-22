{
  config,
  pkgs,
  lib,
  ...
}:
let
  isLinux = pkgs.stdenv.isLinux;
in
{
  options = {
    nx.programs.libreoffice.enable = lib.mkEnableOption "enables libreoffice" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.libreoffice.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        libreoffice
        # language/spellchecker
        hunspell
        hunspellDicts.en_GB-large
      ];
      xdg.mimeApps.defaultApplications = lib.mkIf isLinux {
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document" = [ "writer.desktop" ];
      };
      home.persistence."/persist" = {
        directories = [
          ".config/libreoffice"
        ];
      };
      wayland.windowManager.hyprland.settings =
        lib.mkIf (config.home-manager.users.${config.nx.username}.wayland.windowManager.hyprland.enable)
          {
            window_rule = [
              # prevent libreoffice-writer from fullscreening
              {
                match.class = "libreoffice-writer";
                sync_fullscreen = false;
              }
            ];
          };
    };
  };
}
