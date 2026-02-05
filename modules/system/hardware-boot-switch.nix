{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.system.hardware-boot-switch.enable = lib.mkEnableOption "Code to support printer" // {
      default = false;
    };
  };
  config = lib.mkIf (config.nx.system.hardware-boot-switch.enable && pkgs.stdenv.isLinux) {
    # this module supports a hardware based dual-boot switch
    # It uses a microcontroller which pretends to be a storage device
    # and presents a file with the current switch position in it
    # https://hackaday.io/project/179539-hardware-boot-selection-switch/log/192399-hardware-os-selection-switch

    # NOTE: I no longer dual boot windows, but it seems a shame to get rid of this code, so it sits unused in this module.

    boot.loader.grub = {
      useOSProber = true;
    };

    boot.loader.grub.extraConfig =
      # bash
      ''
        # Look for hardware switch device by its hard-coded filesystem ID
        search --no-floppy --fs-uuid --set hdswitch 55AA-6922
        # If found, read dynamic config file and select appropriate entry for each position
        if [ "''${hdswitch}" ] ; then
          source ($hdswitch)/switch_position_grub.cfg

          if [ "''${os_hw_switch}" == 0 ] ; then
            # Boot Linux
            set default=0
          elif [ "''${os_hw_switch}" == 1 ] ; then
            # Boot Windows
            set default=2
          fi
        fi
      '';
  };
}
