{
  lib,
  config,
  pkgs,
  ...
}:
{
  imports = [
    ./lutris.nix
    ./mednafen.nix
    ./pcsx2.nix
    ./prismlauncher.nix
    ./steam.nix
    ./yuzu.nix
  ];
  options = {
    nx.gaming.enable = lib.mkEnableOption "enables gaming related modules" // {
      default = false;
    };
  };
  config = lib.mkIf (config.nx.gaming.enable && pkgs.stdenv.isLinux) {
    hardware.graphics = {
      enable = true;
      enable32Bit = true;
    };
    programs.gamemode.enable = true;
    home-manager.users.ben = {
      home.persistence."/persist" = {
        directories = [
          ".local/share/vulkan"
        ];
      };
    };
  };
}
