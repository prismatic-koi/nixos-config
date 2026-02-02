{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.udiskie.enable = lib.mkEnableOption "Enable udiskie service, notifications and automounting" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.udiskie.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben.services.udiskie = {
      enable = true;
      automount = true;
      notify = true;
      settings = {
        program_options = {
          menu = "flat";
          file_manager = "xdg-open";
        };
        device_config = [
          # ignore hardware os switch (navi)
          (lib.mkIf (config.networking.hostName == "navi") {
            id_uuid = "55AA-6922";
            ignore = true;
          })
        ];
      };
      tray = "auto";
    };
  };
}
