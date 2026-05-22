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
    hardware.openrazer =
      lib.mkIf (lib.any (d: d.enable && d.kind == "razer") (lib.attrValues cfg.devices))
        {
          enable = true;
          batteryNotifier.enable = false;
        };

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
