{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.gaming.lutris.enable = lib.mkEnableOption "enables lutris" // {
      default = false;
    };
  };
  config =
    lib.mkIf
      (
        config.nx.gaming.lutris.enable
        # only enable if gaming is enabled
        && config.nx.gaming.enable
        && pkgs.stdenv.isLinux
      )
      {
        home-manager.users.ben = {
          home.packages = with pkgs; [
            lutris-unwrapped
          ];
          home.persistence."/persist" = {
            directories = [
              ".local/share/lutris"
            ];
          };
        };
      };
}
