{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.gaming.pscx2.enable = lib.mkEnableOption "enables pscx2, a PlayStation 2 emulator" // {
      default = false;
    };
  };
  config =
    lib.mkIf
      (
        config.nx.gaming.pscx2.enable
        # only enable if gaming is enabled
        && config.nx.gaming.enable
        && pkgs.stdenv.isLinux
      )
      {
        home-manager.users.${config.nx.username} = {
          home.packages = with pkgs; [
            pscx2
          ];
        };
      };
}
