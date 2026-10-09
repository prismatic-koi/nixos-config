{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.gaming.eden.enable = lib.mkEnableOption "Enable Eden" // {
      default = false;
    };
  };
  config = lib.mkIf (config.nx.gaming.eden.enable && pkgs.stdenv.hostPlatform.isLinux) {
    home-manager.users.${config.nx.username} = {
      # pkgs.eden installs its own .desktop file (dev.eden_emu.eden.desktop).
      home.packages = [ pkgs.eden ];
      home.persistence."/persist" = {
        directories = [
          # Eden paths: src/common/fs/path_util.cpp (XDG_DATA_HOME/eden, XDG_CONFIG_HOME/eden)
          ".local/share/eden"
          ".config/eden"
          # Old Yuzu keys, firmware, and saves, kept for import into Eden
          ".local/share/yuzu"
          ".config/yuzu"
        ];
      };
    };
  };
}
