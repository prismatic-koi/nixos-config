{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.services.powerProfilesDaemon.enable = lib.mkEnableOption "Add power-profiles-daemon setup" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.services.powerProfilesDaemon.enable && pkgs.stdenv.isLinux) {
    environment.persistence."/persist/system" = {
      hideMounts = true;
      directories = [
        "/var/lib/power-profiles-daemon" # remember power profile (set using powerprofilesctl set <profile>)
      ];
    };
    services.power-profiles-daemon.enable = true;
  };
}
