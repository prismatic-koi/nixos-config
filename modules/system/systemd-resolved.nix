{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.system.systemdResolved.enable =
      lib.mkEnableOption "systemd-resolved, with NetworkManager pushing per-link upstream DNS servers into it"
      // {
        default = false;
      };
  };

  config = lib.mkMerge [
    {
      assertions = [
        {
          assertion = !(config.nx.system.systemdResolved.enable && config.nx.services.blocky.enable);
          message = ''
            nx.system.systemdResolved.enable and nx.services.blocky.enable are both true.
            blocky sets networking.nameservers = [ "127.0.0.1" ], which conflicts with
            systemd-resolved owning DNS. Enable only one of the two on a given host.
          '';
        }
      ];
    }
    (lib.mkIf (config.nx.system.systemdResolved.enable && pkgs.stdenv.hostPlatform.isLinux) {
      services.resolved.enable = true;
      # NetworkManager does not select the systemd-resolved backend on its own
      # even when services.resolved.enable is true, so set it explicitly.
      networking.networkmanager.dns = "systemd-resolved";
    })
  ];
}
