{
  config,
  lib,
  pkgs,
  ...
}:
let
  configDir = "${config.home-manager.users.ben.home.homeDirectory}/.config";
  resolution = config.nx.desktop.wallpaper.resolution;
  variant = config.nx.desktop.wallpaper.variant;
in
{
  imports = [
    ./enso-wallpaper.nix
    ./rainbox-wallpaper.nix
  ];
  options = {
    nx.desktop.wallpaper.enable = lib.mkEnableOption "enables svg based dynamic wallpaper" // {
      default = true;
    };
    nx.desktop.wallpaper.resolution = lib.mkOption {
      type = lib.types.str;
      default = "5120x1440";
      description = "resolution of the wallpaper";
    };
    nx.desktop.wallpaper.variant = lib.mkOption {
      type = lib.types.str;
      default = "rainbow";
      description = "variant of the wallpaper";
    };
  };
  config = lib.mkIf (config.nx.desktop.wallpaper.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben =
      {
        config,
        lib,
        pkgs,
        ...
      }:
      {
        # convert the svg to a png at activaion time
        home.activation.renderWallpaper = lib.hm.dag.entryAfter [ "writeBoundary" ] ''
          ${pkgs.imagemagick}/bin/convert ${configDir}/${variant}-wallpaper-${resolution}.svg ${configDir}/wallpaper-${resolution}.png
        '';
      };
  };
}
