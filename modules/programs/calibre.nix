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
    nx.programs.calibre.enable = lib.mkEnableOption "enables calibre" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.calibre.enable {
    home-manager.users.ben = {
      home.packages = [
        pkgs.calibre
      ];
      home.persistence."/persist" = {
        directories = [
          ".config/calibre"
        ];
      };
      xdg.mimeApps.defaultApplications = lib.mkIf isLinux {
        "application/epub+zip" = "calibre-ebook-viewer.desktop";
      };
    };
  };
}
