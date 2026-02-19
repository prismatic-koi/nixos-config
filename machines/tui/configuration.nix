{
  pkgs,
  inputs,
  ...
}:
{
  imports = [
    # Include the results of the hardware scan.
    ./hardware-configuration.nix
    (import ./disko.nix { device = "/dev/nvme0n1"; })
    inputs.disko.nixosModules.default
    ../../modules
  ];

  nx = {
    isLaptop = true;
    desktop = {
      theme = "onedark";
      hyprland = {
        lockTimeout.enable = false;
        screenTimeout.duration = 600; # screen off after 10 minutes
        suspendTimeout.duration = 900; # suspend after 15 minutes
      };
      # hyprlock.oled = true;
      wallpaper = {
        variant = "enso";
        resolution = "2880x1800";
      };
      offline-focus-mode.enable = true;
    };
    programs = {
      chromium.enable = true;
      calibre.enable = true;
      cura.enable = true;
      darktable.enable = true;
      firefox.hideUrlbar = true;
      gcalcli.enable = true;
      homeAutomation.enable = true;
      libreoffice.enable = true;
      obsidian.enable = true;
      picard.enable = true;
      plexamp.enable = true;
      virt-manager.enable = true;
      wgnord.enable = true;
    };
    services = {
      blocky.enable = false; # for roaming, slows startup at home
      syncthing = {
        enable = true;
        obsidian.enable = true;
        calibre.enable = true;
        music.enable = true;
        photos.enable = true;
        darktable.enable = true;
      };
      printer.enable = true;
    };
    system = {
      nfs-mounts.enable = true;
      sops.ageKeys.enable = true;
    };
    gaming = {
      enable = true;
      prismlauncher.enable = true;
      yuzu.enable = true;
    };
  };

  # Bootloader.
  boot.loader = {
    systemd-boot.enable = true;
    efi.canTouchEfiVariables = true;
  };

  # Fix suspend/resume issues
  boot.kernelParams = [
    "mem_sleep_default=s2idle" # Use s2idle instead of deep sleep
    "nvme.noacpi=1"
    "acpi_osi=Linux"
    "i915.enable_psr=0" # Disable panel self refresh for Intel graphics
    "i915.enable_fbc=0" # Disable framebuffer compression
  ];

  # Additional power management
  powerManagement = {
    enable = true;
    powertop.enable = true;
  };

  # Additional hardware support for suspend/resume
  services.udev.extraRules = ''
    # Disable USB autosuspend for devices that might cause issues
    ACTION=="add", SUBSYSTEM=="usb", ATTR{idVendor}=="8087", ATTR{power/autosuspend}="-1"
  '';

  # Ensure proper ACPI handling
  services.acpid.enable = true;

  networking.hostName = "tui";

  # home-manager is awesome
  home-manager = {
    useGlobalPkgs = true;
    useUserPackages = true;
    extraSpecialArgs = {
      inherit inputs;
    };
    users = {
      ben.home = {
        username = "ben";
        homeDirectory = "/home/ben";
        stateVersion = "24.05"; # Do Not Touch!
      };
    };
  };

  # display settigs for hyprland
  home-manager.users.ben.wayland.windowManager.hyprland.settings.monitor = [
    "eDP-1,2880x1800@120.00000,0x0,1.5"
  ];

  # List packages installed in system profile. To search, run:
  # $ nix search wget
  environment.systemPackages = with pkgs; [
    cheese
    exfat
    ffmpeg
    freetype # fonts needed for wine
    gamescope
    gphoto2
    inkscape
    lm_sensors
    ntfs3g
    openscad
    parted
    protontricks
    protonup-ng
    shotcut
    solvespace
    usbutils
    v4l-utils
    wev
    wineWow64Packages.waylandFull
  ];
  # key remapping
  services.keyd = {
    enable = true;
    keyboards = {
      main = {
        settings = {
          main = {
            capslock = "overload(control,esc)";
          };
        };
      };
      mouse = {
        ids = [ "1532:00b7:aa6166ef" ]; # Razer Deathadder V3 Pro
        settings = {
          main = {
            mouse1 = "volumedown";
            mouse2 = "volumeup";
          };
        };
      };
    };
  };

  services.udisks2 = {
    enable = true;
    mountOnMedia = true;
  };

  # This value determines the NixOS release from which the default
  # settings for stateful data, like file locations and database versions
  # on your system were taken. It‘s perfectly fine and recommended to leave
  # this value at the release version of the first install of this system.
  # Before changing this value read the documentation for this option
  # (e.g. man configuration.nix or on https://nixos.org/nixos/options.html).
  system.stateVersion = "24.05"; # Did you read the comment?
}
