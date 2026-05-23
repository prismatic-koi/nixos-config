{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config.nx.services.batteryNotifier;

  enabledDevices = lib.filterAttrs (_: d: d.enable) cfg.devices;

  # Emit a single JSON config describing every enabled device. The
  # daemon (battery-notifier --config <path>) reads this once at
  # startup; a NixOS rebuild restarts the user unit so live reload
  # is unnecessary.
  daemonConfigJSON = builtins.toJSON {
    devices = lib.mapAttrsToList (name: dev: {
      inherit name;
      kind = dev.kind;
      lowThreshold = dev.lowThreshold;
      fullThreshold = dev.fullThreshold;
      dismissThreshold = dev.dismissThreshold;
      ignoreZero = dev.ignoreZero;
    }) enabledDevices;
  };

  daemonConfigFile = pkgs.writeText "battery-notifier-config.json" daemonConfigJSON;

  anyDeviceEnabled = enabledDevices != { };

  anyRazerDevice = lib.any (d: d.enable && d.kind == "razer") (lib.attrValues cfg.devices);

  # openrazer's shipped udev rule (99-razer.rules) sets GROUP:=openrazer on
  # the usb|input|hid device nodes, but the razermouse kernel module creates
  # the per-attribute files (charge_level, charge_status) inside the device's
  # sysfs directory as root:root 0440. Those files are what battery-notifier
  # reads — and because the user (in the openrazer group) is not the owner
  # and the group is root, every read returns EACCES.
  #
  # This wrapper runs from a udev RUN+= when an idVendor==1532 hid device
  # appears and walks the sysfs directory chgrp/chmod'ing any battery-related
  # attribute files that exist. Using a wrapper rather than inlining the
  # commands into RUN+= avoids the well-known fragility of shell escaping
  # and %p expansion inside udev rule strings (see issue #1972).
  #
  # The script is defensive: it tolerates the attributes being absent (not
  # every razer device exposes a battery) and tolerates udev firing the rule
  # before the kernel module has created the sysfs files (a short retry loop
  # handles the race).
  razerFixPermsScript = pkgs.writeShellScript "razer-fix-charge-perms" ''
    # $DEVPATH is exported by udev, e.g. /devices/pci0000:00/.../0003:1532:00B7.0007
    sysfs="/sys$DEVPATH"
    # Try a few times in case the kernel module hasn't created the
    # attribute files yet when udev fires this rule.
    for _ in 1 2 3 4 5; do
      if [ -e "$sysfs/charge_level" ] || [ -e "$sysfs/charge_status" ]; then
        for f in "$sysfs"/charge_level "$sysfs"/charge_status; do
          [ -e "$f" ] || continue
          ${pkgs.coreutils}/bin/chgrp openrazer "$f" || true
          ${pkgs.coreutils}/bin/chmod g+r "$f"     || true
        done
        exit 0
      fi
      ${pkgs.coreutils}/bin/sleep 0.2
    done
    # No battery attributes appeared — not an error (most razer devices
    # don't have a battery).
    exit 0
  '';
in
{
  options.nx.services.batteryNotifier = {
    devices = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule (
          { name, ... }:
          {
            options = {
              enable = lib.mkEnableOption "this battery notifier";

              # `kind` selects the data source the Go daemon uses.
              # "laptop" → UPower on the system bus (PropertiesChanged
              # subscription). "razer" → /sys/bus/hid/devices/*1532*/
              # charge_level + charge_status, polled internally on a
              # 1-minute ticker. See DESIGN.md.
              kind = lib.mkOption {
                type = lib.types.enum [
                  "laptop"
                  "razer"
                ];
                description = "Battery data source: 'laptop' (UPower) or 'razer' (sysfs).";
              };

              lowThreshold = lib.mkOption {
                type = lib.types.int;
                default = 20;
                description = "The battery percentage to trigger a low battery notification.";
              };

              fullThreshold = lib.mkOption {
                type = lib.types.int;
                default = 100;
                description = "The battery percentage to trigger a full battery notification.";
              };

              dismissThreshold = lib.mkOption {
                type = lib.types.int;
                default = 50;
                description = "The battery percentage at which to dismiss the low battery notification.";
              };

              ignoreZero = lib.mkOption {
                type = lib.types.bool;
                default = false;
                description = "Whether to ignore erroneous 0% battery readings.";
              };
            };
          }
        )
      );
      default = { };
      description = ''
        Configuration for batteries to monitor.

        NOTE: the previous `reNotifyThreshold` option was removed in the
        Go-daemon rewrite. The "fully charged" notification now fires
        once per Discharging→Charging transition rather than gating on a
        re-notify percentage. See
        modules/services/battery-notifier/DESIGN.md for the rationale.
      '';
    };
  };

  config = lib.mkIf pkgs.stdenv.isLinux {
    # openrazer remains the kernel-side driver for the Razer mouse.
    # It exposes charge_level / charge_status under
    # /sys/bus/hid/devices/*1532*/ which the daemon reads directly —
    # no Polychromatic-CLI subprocess. openrazer's own batteryNotifier
    # is left disabled because we own that responsibility now.
    hardware.openrazer = lib.mkIf anyRazerDevice {
      enable = true;
      batteryNotifier.enable = false;
    };

    # Fix sysfs perms on razer battery attribute files. See the comment on
    # `razerFixPermsScript` above for the root-cause explanation. Gated on
    # the same condition as `hardware.openrazer.enable` so hosts with no
    # razer device declared get no extra udev rule.
    services.udev.extraRules = lib.mkIf anyRazerDevice ''
      ACTION=="add", SUBSYSTEM=="hid", ATTRS{idVendor}=="1532", RUN+="${razerFixPermsScript}"
    '';

    home-manager.users.${config.nx.username} = lib.mkIf anyDeviceEnabled {
      # polychromatic is intentionally NOT installed — the Go daemon
      # talks to sysfs (with an openrazer D-Bus fallback path in the
      # source) and does not need the Polychromatic CLI.
      home.packages = with pkgs; [
        dunst
        libnotify
      ];

      # One long-running user service. No timer, no udev, no
      # machinectl wrapper. Restart=on-failure means a crash gets a
      # fresh process; PartOf=graphical-session.target ties the
      # daemon's lifetime to the user's desktop session.
      systemd.user.services.battery-notifier = {
        Unit = {
          Description = "Battery notifier (Go daemon: UPower + sysfs)";
          After = [ "graphical-session.target" ];
          PartOf = [ "graphical-session.target" ];
        };
        Service = {
          Type = "simple";
          ExecStart = "${pkgs.battery-notifier}/bin/battery-notifier --config ${daemonConfigFile} --log-format text";
          Restart = "on-failure";
          RestartSec = 5;
        };
        Install = {
          WantedBy = [ "graphical-session.target" ];
        };
      };
    };

    # Default device definitions for this machine. Consumers can
    # override per-device settings via `nx.services.batteryNotifier.devices.*`.
    nx.services.batteryNotifier.devices = {
      laptop = {
        enable = config.nx.isLaptop;
        kind = "laptop";
        lowThreshold = 20;
        dismissThreshold = 50;
      };
      mouse = {
        enable = true;
        kind = "razer";
        lowThreshold = 20;
        dismissThreshold = 50;
        ignoreZero = true;
      };
    };
  };
}
