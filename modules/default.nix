{
  config,
  lib,
  pkgs,
  ...
}:
{
  imports = [
    ./colour-scheme
    ./desktop
    ./gaming
    ./programs
    ./services
    ./system
  ];
  options = {
    nx.username = lib.mkOption {
      default = "ben";
      description = "primary username for this machine";
      type = lib.types.str;
    };
    nx.externalAudio.enable = lib.mkEnableOption {
      default = false;
      description = "machine is using external audio control, disable things like volume controls";
    };
    nx.deviceLocation = lib.mkOption {
      default = "none";
      description = "physical location of the machine, for showing local variables like temp humidity";
      type = lib.types.str;
    };
    nx.isLaptop = lib.mkEnableOption {
      default = false;
      description = "machine is a laptop, enable things like battery monitoring";
    };
  };
  config = {
    # Let Home Manager install and manage itself.
    home-manager.users.${config.nx.username}.programs.home-manager.enable = true;
  };
}
