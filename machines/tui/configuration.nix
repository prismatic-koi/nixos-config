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
      theme = "edge";
      hyprland = {
        lockTimeout.enable = false;
        screenTimeout.duration = 600; # screen off after 10 minutes
        suspendTimeout.duration = 900; # suspend after 15 minutes
      };
      # hyprlock.oled = true;
      wallpaper.variant = "enso-6colour";
    };
    programs = {
      prism = {
        profile.default = "standard";
        agent.isolation.default = "bwrap";
        pi.notion = {
          enable = true;
          repos = [ "~/documents/obsidian" ];
        };
        pi.grafana = {
          enable = true;
          config = "home";
        };
      };
      chromium.enable = true;
      calibre.enable = true;
      darktable.enable = true;
      firefox.hideUrlbar = true;
      gcalcli.enable = true;
      homeAutomation.enable = true;
      obsidian.enable = true;
      picard.enable = true;
      wgnord.enable = true;
    };
    services = {
      flakeUpdateNotifier = {
        enable = true;
        notifyOnWake = true;
      };
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
    # Same conservative cap as navi. tui declares 1G in disko.nix but
    # disko does not retroactively resize an installed partition — navi
    # is the proof case: disko.nix declares 1G, /dev/nvme0n1p2 on the
    # host is still 500M. Until tui's actual ESP is measured (df -h
    # /boot on the host) we assume it may also still be 500M. On
    # unstable each kernel+initrd set costs ~92M (14M bzImage + 78M
    # initrd, measured on navi); 4 sets ~368M plus one incoming ~78M
    # copy during a switch fits any ESP >= 500M. systemd-boot has the
    # same copy-before-prune failure mode as grub's install-grub.pl, so
    # once the ESP fills, every subsequent switch fails at the copy and
    # the prune never runs. Do not bump this back up without first
    # measuring the actual ESP and re-deriving the maths.
    systemd-boot.configurationLimit = 4;
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
        stateVersion = "25.11";
      };
    };
  };

  # display settigs for hyprland
  home-manager.users.ben.wayland.windowManager.hyprland.settings.monitor = [
    {
      output = "eDP-1";
      mode = "2880x1800@120.00000";
      position = "0x0";
      scale = 1.5;
    }
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
    openscad-unstable
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
  system.stateVersion = "25.11";
}
