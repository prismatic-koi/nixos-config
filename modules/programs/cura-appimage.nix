# Override to make sure cura is most recent version
{
  config,
  pkgs,
  lib,
  ...
}:
let
  cura-appimage = (
    let
      app_name = "cura";
      gh_user = "Ultimaker";
      gh_proj = "Cura";
      version = "5.9.0";
      hash = "17h2wy2l9djzcinmnjmi2c7d2y661f6p1dqk97ay7cqrrrw5afs9";
    in
    pkgs.appimageTools.wrapType2 {
      pname = app_name;
      version = version;
      src = builtins.fetchurl {
        url =
          "https://github.com/${gh_user}/${gh_proj}/releases/download/${version}/"
          + "${gh_user}-${gh_proj}-${version}-linux-X64.AppImage";
        sha256 = "${hash}";
      };
    }
  );
in
{
  options = {
    nx.programs.cura.enable = lib.mkEnableOption "Enable Cura" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.cura.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = [
        cura-appimage
      ];
      home.persistence."/persist" = {
        directories = [
          ".config/cura"
          ".local/share/cura"
        ];
      };
      xdg.desktopEntries = lib.mkIf pkgs.stdenv.isLinux {
        cura =
          let
            cura-icon = pkgs.fetchurl {
              url = "https://raw.githubusercontent.com/Ultimaker/Cura/main/packaging/icons/cura-icon.svg";
              sha256 = "sha256-ypqrZ/Wv/a+oyYHYtP8Aa/BeOBelTBoLcDEauiViJ7I=";
            };
          in
          {
            name = "Cura";
            genericName = "3D Printing Software";
            exec = "cura %U";
            icon = cura-icon;
            terminal = false;
            categories = [ "Utility" ];
          };
      };
      xdg.mimeApps.defaultApplications = {
        "model/stl" = [ "cura.desktop" ];
        "application/vnd.ms-3mfdocument" = [ "cura.desktop" ];
        "application/prs.wavefront-obj" = [ "cura.desktop" ];
        "text/x-gcode" = [ "cura.desktop" ];
      };
    };
  };
}
