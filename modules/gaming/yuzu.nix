{
  config,
  pkgs,
  lib,
  ...
}:
let
  yuzu-appimage = (
    let
      pname = "yuzu";
      version = "EA-4176";
      url = "https://raw.githubusercontent.com/vvsx87/yuzu-binaries/refs/heads/master/Linux-Yuzu-EA-4176.AppImage";
      sha256 = "sha256-bUTVL8br2POy5HB1FszlNQNChdRWcwIlG6/RCceXIlg=";
    in
    pkgs.appimageTools.wrapType2 {
      pname = pname;
      version = version;
      src = builtins.fetchurl {
        url = url;
        sha256 = sha256;
      };
    }
  );
in
{
  options = {
    nx.gaming.yuzu.enable = lib.mkEnableOption "Enable Yuzu" // {
      default = false;
    };
  };
  config = lib.mkIf (config.nx.gaming.yuzu.enable && pkgs.stdenv.isLinux) {
    home-manager.users.${config.nx.username} = {
      home.packages = [
        yuzu-appimage
      ];
      home.persistence."/persist" = {
        directories = [
          ".local/share/yuzu"
          ".config/yuzu"
        ];
      };
      xdg.desktopEntries = lib.mkIf pkgs.stdenv.isLinux {
        yuzu =
          let
            yuzu-icon = pkgs.fetchurl {
              url = "https://upload.wikimedia.org/wikipedia/commons/3/35/Yuzu_Emulator.svg";
              sha256 = "sha256-WnbfybSphPat4Hrn7DEWBVQmpVndgmNH7Q0EFfdwac0=";
            };
          in
          {
            name = "Yuzu";
            genericName = "Nintendo Switch Emulator";
            exec = "yuzu %U";
            icon = yuzu-icon;
            terminal = false;
            categories = [
              "Game"
              "Emulator"
            ];
          };
      };
    };
  };
}
