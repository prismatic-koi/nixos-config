{
  pkgs,
  inputs,
  ...
}:
let
  # ── GPU driver-binding detection metric (i915 vs xe) ────────────────
  #
  # The GPU is Meteor Lake-P [8086:7d55]. Both `i915` and `xe` claim
  # this device ID, and upstream is progressively defaulting newer
  # generations to `xe`. If `xe` ever binds this panel, the four
  # `i915.*` boot params below go SILENTLY inert — `xe` has no
  # `enable_psr` / `enable_fbc` knobs — and the suspend/resume
  # mitigation they provide vanishes with no visible error.
  #
  # This oneshot records which kernel driver is bound to each DRM card
  # so the flip is observable in Prometheus. It writes a node-exporter
  # textfile metric into the same directory the Alloy `textfile`
  # collector reads (`/var/lib/prometheus-node-exporter-textfile`,
  # created world-readable by modules/services/alloy). Pattern copied
  # from `systemdUserUnitTextfileScript` in that module: write to a
  # temp file, chmod 0644, atomic rename.
  #
  # Metric: node_gpu_driver_bound{card,pci,driver} = 1 (one series per
  # DRM card). Alert seam: on tui, node_gpu_driver_bound{driver="xe"}
  # firing — or the i915 series going absent — means the flip has
  # happened and the i915.* params are now inert.
  gpuDriverMetricsScript = pkgs.writeShellScript "gpu-driver-metrics" ''
    set -euo pipefail
    dir="/var/lib/prometheus-node-exporter-textfile"
    final="$dir/gpu-driver.prom"
    tmp="$final.$$"
    {
      echo "# HELP node_gpu_driver_bound Kernel driver bound to a GPU DRM card (value always 1; read the driver label). Detects an i915->xe flip that silently disables the i915.enable_psr/enable_fbc boot params."
      echo "# TYPE node_gpu_driver_bound gauge"
      for card in /sys/class/drm/card*; do
        name=$(${pkgs.coreutils}/bin/basename "$card")
        # Skip connector subdirs like card1-eDP-1; keep real cards only.
        case "$name" in
          *-*) continue ;;
        esac
        [ -e "$card/device/driver" ] || continue
        driver=$(${pkgs.coreutils}/bin/basename "$(${pkgs.coreutils}/bin/readlink -f "$card/device/driver")")
        pci=$(${pkgs.coreutils}/bin/basename "$(${pkgs.coreutils}/bin/readlink -f "$card/device")")
        echo "node_gpu_driver_bound{card=\"$name\",pci=\"$pci\",driver=\"$driver\"} 1"
      done
    } > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv -f "$tmp" "$final"
  '';
in
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
      tailscaleClient.enable = true;
      wgnord.enable = true;
    };
    services = {
      alloy.enable = true;
      flakeUpdateNotifier = {
        enable = true;
        notifyOnWake = true;
      };
      blocky.enable = false; # for roaming, slows startup at home
      prismExporter.enable = true;
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

  # Fix suspend/resume issues.
  #
  # tui froze on suspend/resume in late 2025. Two commits mitigated it:
  # 0db17174 (2025-10-24) added mem_sleep_default=s2idle, nvme.noacpi=1,
  # acpi_osi=Linux (+ powertop/PM); ef385823 (2025-11-02) added
  # i915.enable_psr=0, i915.enable_fbc=0 and the USB udev rule below.
  # The freeze stopped, but nobody isolated WHICH change fixed it.
  #
  # Driver binding VERIFIED as i915 on 2026-08-21 (kernel 7.1.8):
  #   lspci -nnk -d ::0300 -> "Kernel driver in use: i915"
  #                           "Kernel modules: i915, xe"
  #   /sys/class/drm/card1/device/driver -> .../drivers/i915
  #   enable_psr=0, enable_fbc=0 both took (root reads; files are 0400).
  # The panel supports PSR (Sink support: PSR = yes [0x03]) but PSR mode
  # is disabled — the param is actively suppressing a feature the panel
  # supports. ~121 clean suspend cycles across boots 0..-4, every one
  # of them run with PSR/FBC OFF. That proves the CURRENT config is
  # stable; it does NOT prove the params are unnecessary (testing the
  # counter-factual needs a real suspend, which is human-only).
  #
  # i915.* KEPT (not removed) because the upstream fix-history is
  # INCONCLUSIVE. Meteor Lake (Xe-LPG, gen 12.5) PSR/Panel-Replay is
  # still an area of active churn in 2026: upstream keeps adding
  # per-machine DISABLE quirks (e.g. 5e79af5db00b "Disable PSR2 on
  # Xiaomi Book Pro 14 2026", 45c77d4bf8d4 "Disable Panel Replay on
  # Dell XPS 14 DA14260"), and the resume-path DC6/vblank fixes
  # (35485ac56d87, eb5911f99055; Cc stable v6.13+/v6.16+) address
  # warnings and refcounts rather than a definitive "MTL eDP
  # freeze-on-resume is fixed" statement. No citable FBC MTL
  # suspend/resume regression fix was found at all. So the params stay,
  # and the gpu-driver-metrics oneshot below detects an i915->xe flip.
  #
  # xe is a candidate for THIS device: both i915 and xe claim
  # [8086:7d55]. If xe ever binds, all four i915.* tokens go inert
  # (xe has no enable_psr/enable_fbc). Detect that flip; never forbid
  # it (no modprobe.blacklist=xe, no force_probe).
  boot.kernelParams = [
    "mem_sleep_default=s2idle" # Use s2idle instead of deep sleep
    "nvme.noacpi=1"
    "acpi_osi=Linux"
    "i915.enable_psr=0" # Disable panel self refresh for Intel graphics
    "i915.enable_fbc=0" # Disable framebuffer compression
  ];

  # Record the bound GPU driver (i915 vs xe) as a node-exporter
  # textfile metric at boot. See gpuDriverMetricsScript in the `let`
  # block above for the rationale. Depends on the textfile directory
  # created by modules/services/alloy (nx.services.alloy.enable = true).
  systemd.services.gpu-driver-metrics = {
    description = "Record the bound GPU DRM driver as a node-exporter textfile metric";
    wantedBy = [ "multi-user.target" ];
    after = [ "systemd-tmpfiles-setup.service" ];
    serviceConfig = {
      Type = "oneshot";
      ExecStart = "${gpuDriverMetricsScript}";
    };
  };

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
