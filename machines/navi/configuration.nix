{
  pkgs,
  inputs,
  config,
  lib,
  ...
}:
{
  imports = [
    # Include the results of the hardware scan.
    ./hardware-configuration.nix
    (import ./disko.nix { device = "/dev/nvme1n1"; })
    inputs.disko.nixosModules.default
    ../../modules
  ];

  nx = {
    externalAudio.enable = true; # using external dac
    deviceLocation = "office";
    desktop = {
      theme = "everforest";
      hyprland = {
        layout = "dwindle";
        disableWorkspaceAnimations = true;
      };
      wallpaper.variant = "enso";
    };
    programs = {
      anki.enable = false; # build broken as of 2025-08-30
      calibre.enable = true;
      claude-code.enable = true;
      cura.enable = true;
      darktable.enable = true;
      firefox = {
        enable = true;
        hideUrlbar = true;
      };
      gcalcli.enable = true;
      homeAutomation.enable = true;
      libreoffice.enable = true;
      obsidian.enable = true;
      picard.enable = true;
      plexamp.enable = true;
      rclone.enable = true;
      voiceToText.enable = true;
      waydroid.enable = false;
      wgnord.enable = true;
    };
    services = {
      syncthing = {
        enable = true;
        obsidian.enable = true;
        music = {
          enable = true;
          path = "/home/ben/music";
        };
        photos.enable = true;
        darktable.enable = true;
      };
      printer.enable = true;
    };
    system = {
      nfs-mounts.enable = true;
      sops = {
        ageKeys.enable = true;
      };
      hardware-boot-switch.enable = true;
    };
    gaming = {
      enable = true;
      lutris.enable = false; # isnt able to build 2025-11-08
      # we don't need to persist steam
      # it already has its own drive on this machine
      steam.persist = false;
      prismlauncher.enable = true;
    };
  };

  # Bootloader.
  boot.loader.grub = {
    enable = true;
    efiSupport = true;
    efiInstallAsRemovable = true;
    configurationLimit = 20; # Keep only 20 most recent generations in /boot
  };

  networking.hostName = "navi";

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

  services.hardware.openrgb.enable = true;

  # display settigs for hyprland
  home-manager.users.ben.wayland.windowManager.hyprland.settings = {
    monitor = [
      "DP-3,5120x1440@239.76Hz,0x0,1"
    ];
    plugin.hy3.autotile.trigger_width = 1280;
  };

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
    mangohud
    ntfs3g
    openscad
    parted
    protontricks
    protonup-ng
    scanmem
    shotcut
    solvespace
    usbutils
    v4l-utils
    wineWow64Packages.waylandFull
  ];

  # key remapping
  services.keyd = {
    enable = true;
    keyboards = {
      mouse = {
        ids = [ "1532:00b7:aa6166ef" ]; # Razer Deathadder V3 Pro
        settings = {
          main = {
            mouse1 = "playpause";
            mouse2 = "leftmeta";
          };
        };
      };
    };
  };

  # sound
  services.pipewire.wireplumber.extraConfig = {
    # 100% volume
    "10-default-volume" = {
      "wireplumber.settings"."device.routes.default-sink-volume" = 1.0;
    };
    # FiiO K7 as default sink
    "10-default-sink" = {
      "wireplumber.settings"."default.configured.audio.sink" =
        "alsa_output.usb-GuangZhou_FiiO_Electronics_Co._Ltd_FiiO_K7-00.analog-stereo";
    };
    # AT2020USB-XLR as default source
    "10-default-source" = {
      "wireplumber.settings"."default.configured.audio.source" =
        "alsa_input.usb-AT_AT2020USB-X_202011110001-00.mono-fallback";
    };
    "51-alsa-disable" = {
      "monitor.alsa.rules" = [
        {
          "matches" = [
            {
              # disable, not a output device
              "node.name" = "alsa_output.usb-AT_AT2020USB-X_202011110001-00.analog-stereo";
            }
            {
              # Bult in output disable, nothing plugged in
              "node.name" = "alsa_output.pci-0000_00_1f.3.iec958-stereo";
            }
            {
              # HDMI output disable, never used
              "node.name" = "alsa_output.pci-0000_03_00.1.hdmi-stereo-extra4";
            }
            {
              # disable, webcam should never be used for audio
              "node.name" = "alsa_input.usb-046d_Logitech_StreamCam_F2867D05-02.analog-stereo";
            }
            {
              # Bult in input disable, nothing plugged in
              "node.name" = "alsa_input.pci-0000_00_1f.3.analog-stereo";
            }
          ];
          "actions" = {
            "update-props" = {
              "node.disabled" = true;
            };
          };
        }
      ];
    };
  };

  services.udisks2 = {
    enable = true;
    mountOnMedia = true;
  };

  # programs.virt-manager.enable = true;
  networking.firewall = {
    allowedTCPPorts = [
      6600 # mpd
    ];
  };

  # This value determines the NixOS release from which the default
  # settings for stateful data, like file locations and database versions
  # on your system were taken. It‘s perfectly fine and recommended to leave
  # this value at the release version of the first install of this system.
  # Before changing this value read the documentation for this option
  # (e.g. man configuration.nix or on https://nixos.org/nixos/options.html).
  system.stateVersion = "24.05"; # Did you read the comment?
}
