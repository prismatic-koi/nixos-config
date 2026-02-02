{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.services.printer.enable = lib.mkEnableOption "Configuration to support printer" // {
      default = false;
    };
  };
  config = lib.mkIf (config.nx.services.printer.enable && pkgs.stdenv.isLinux) {
    # cups for printing
    services.printing = {
      enable = true;
      drivers = [
        # Brother laser printer driver
        pkgs.brlaser
      ];
    };
    # printer autoddiscovery
    services.avahi = {
      enable = true;
      nssmdns4 = true;
      openFirewall = true;
    };
    # Define the Brother HL-1210W printer declaratively
    hardware.printers = {
      ensurePrinters = [
        {
          name = "Brother_HL-1210W_series";
          description = "Brother HL-1210W series";
          deviceUri = "dnssd://Brother%20HL-1210W%20series._pdl-datastream._tcp.local/?uuid=e3248000-80ce-11db-8000-b4b5b6e34d51";
          model = "drv:///brlaser.drv/br1210.ppd";
          ppdOptions = {
            PageSize = "A4";
          };
        }
      ];
      ensureDefaultPrinter = "Brother_HL-1210W_series";
    };
  };
}
