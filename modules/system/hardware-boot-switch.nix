{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.system.hardware-boot-switch.enable = lib.mkEnableOption "Hardware dual-boot switch support" // {
      default = false;
    };
  };
  config = lib.mkIf (config.nx.system.hardware-boot-switch.enable && pkgs.stdenv.isLinux) {
    # this module supports a hardware based dual-boot switch
    # It uses a microcontroller which pretends to be a storage device
    # and presents a file with the current switch position in it
    # https://hackaday.io/project/179539-hardware-boot-selection-switch/log/192399-hardware-os-selection-switch

    boot.loader.grub = {
      useOSProber = true;
    };

    boot.loader.grub.extraConfig =
      # bash
      ''
        # Load USB and filesystem modules needed to find the hardware switch device
        insmod usb
        insmod usbms
        insmod fat
        insmod search_fs_uuid
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
            set default='osprober-efi-1F84-B7DC'
          fi
        fi
      '';
  };
}
