{
  config,
  pkgs,
  lib,
  ...
}:
let
  externalAudio = config.nx.externalAudio.enable;
  enableHomeAutomation = config.nx.programs.homeAutomation.enable;
  location = config.nx.deviceLocation;
  enableMpd = config.nx.services.mpd.enable;
  enableHyprland = config.nx.desktop.hyprland.enable;
  enableSway = config.home-manager.users.ben.wayland.windowManager.sway.enable;
  homeDir = config.home-manager.users.ben.home.homeDirectory;
  isLaptop = config.nx.isLaptop;
in
with config.theme;
{
  options = {
    nx.desktop.waybar = {
      enable = lib.mkEnableOption "enables waybar" // {
        default = true;
      };
    };
  };
  imports = [
    ./scripts.nix
  ];
  config = lib.mkIf (config.nx.desktop.waybar.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben.programs.waybar = {
      enable = true;
      package = pkgs.waybar;
      systemd.target = "sway-session.target";
      settings = {
        topBar = {
          name = "topBar";
          layer = "top";
          position = "top";
          height = 40;
          mode = "overlay";
          start_hidden = true;
          margin = "30"; # margin for the whole bar
          # modules-left = [
          #   (lib.mkIf enableSway "sway/workspaces")
          #   (lib.mkIf enableSway "sway/scratchpad")
          #   (lib.mkIf enableSway "sway/mode")
          #   (lib.mkIf enableHyprland "hyprland/workspaces")
          #   (lib.mkIf enableHyprland "hyprland/submap")
          # ];
          modules-center = [
            "hyprland/window"
          ];
          modules-right = [
            "tray"
            (lib.mkIf (externalAudio == false) "pulseaudio")
            (lib.mkIf externalAudio "custom/audio-enabled")
            (lib.mkIf (enableHomeAutomation && location == "office") "custom/office-temp")
            (lib.mkIf (enableHomeAutomation && location == "office") "custom/office-humidity")
            "custom/notification"
            "custom/inhibitidle"
            "custom/clock"
          ];
          "sway/workspaces" = {
            "all-outputs" = false;
            "disable-scroll" = true;
          };
          "sway/scratchpad" = {
            "format" = "{icon} {count}";
            "show-empty" = false;
            "format-icons" = [
              ""
              "󰎝"
            ];
            "tooltip" = true;
            "tooltip-format" = "{app}: {title}";
          };
          "hyprland/workspaces" = {
            "persistent-workspaces" = {
              "*" = 9;
            };
          };
          "hyprland/window" = {
            "format" = "{title}";
            "rewrite" = {
              # remove the browser name from the title
              "(.*) — Mozilla Firefox" = "$1";
              "(.*) - qutebrowser" = "$1";
            };
          };
          "custom/office-temp" = lib.mkIf enableHomeAutomation {
            "return-type" = "string";
            "interval" = 60;
            "format" = " {}";
            "exec" = "${homeDir}/.local/scripts/cli.home.office.getTemperature";
          };
          "custom/office-humidity" = lib.mkIf enableHomeAutomation {
            "return-type" = "string";
            "interval" = 60;
            "format" = " {}";
            "exec" = "${homeDir}/.local/scripts/cli.home.office.getHumidity";
          };
          "tray" = {
            "icon-size" = 18;
            "spacing" = 12;
          };
          "pulseaudio" = lib.mkIf (externalAudio == false) {
            "format" = "󰕾 {volume}%";
            "format-muted" = "󰖁 {volume}%";
            "on-click" = "qpwgraph";
          };
          "custom/audio-enabled" = {
            "return-type" = "string";
            "interval" = 1;
            "exec" = "${homeDir}/.local/scripts/cli.audio.outputEnabled";
          };
          "custom/inhibitidle" = {
            "return-type" = "json";
            "interval" = 1;
            "exec" = "${homeDir}/.local/scripts/cli.system.inhibitIdle statusjson";
            "format" = "{}";
          };
          "custom/clock" = {
            "return-type" = "string";
            "interval" = 1;
            "exec" = "date +'%F %T'";
          };
          "custom/notification" = {
            "tooltip" = false;
            "format" = "{icon}";
            "format-icons" = {
              "notification" = "<span foreground='${red}'><sup></sup></span>";
              "none" = "";
              "dnd-notification" = "<span foreground='${red}'><sup></sup></span>";
              "dnd-none" = "";
            };
            "return-type" = "json";
            "exec-if" = "which swaync-client";
            "exec" = "swaync-client -swb";
            "on-click" = "swaync-client -t -sw";
            "on-click-right" = "swaync-client -d -sw";
            "escape" = true;
          };
        };
        bottomBar = {
          name = "bottomBar";
          layer = "top";
          position = "bottom";
          height = 40;
          mode = "overlay";
          start_hidden = true;
          margin = "30"; # margin for the whole bar
          # modules-left = [
          #   (lib.mkIf enableMpd "custom/mpd")
          # ];
          modules-center = [
            (lib.mkIf config.nx.desktop.offline-focus-mode.enable "custom/system-mode")
          ];
          modules-right = [
            "cpu"
            "memory"
            "network"
            (lib.mkIf config.nx.programs.wgnord.enable "custom/vpn-status")
            (lib.mkIf isLaptop "custom/battery")
            "power-profiles-daemon"
            "systemd-failed-units"
          ];
          "custom/system-mode" = {
            "return-type" = "json";
            "interval" = 1;
            "exec" = "${homeDir}/.local/scripts/cli.desktop.getSystemMode json";
            "format" = "{}";
          };

          "cpu" = {
            "format" = " {usage}%";
            "interval" = 10;
          };
          "memory" = {
            "format" = " {percentage}%";
            "interval" = 10;
          };
          "network" = {
            "format-ethernet" = "󰈀 {ifname}";
            "format-wifi" = "󱚽 {essid} ({signalStrength}%)";
          };
          "custom/vpn-status" = lib.mkIf config.nx.programs.wgnord.enable {
            "return-type" = "json";
            "interval" = 5;
            "exec" = "${homeDir}/.local/scripts/cli.system.vpnStatus json";
            "format" = "{}";
            "tooltip" = true;
          };
          "custom/mpd" = lib.mkIf enableMpd {
            "return-type" = "string";
            "interval" = 1;
            "exec" = "${homeDir}/.local/scripts/cli.mpd.nowPlaying";
          };
          "custom/battery" = lib.mkIf isLaptop {
            "return-type" = "string";
            "interval" = 5;
            "exec" = "${homeDir}/.local/scripts/cli.system.batteryStatus";
          };
          "power-profiles-daemon" = {
            "format" = "{icon}";
            "format-icons" = {
              "default" = " ";
              "performance" = " ";
              "balanced" = " ";
              "power-saver" = " ";
            };
          };
          "systemd-failed-units" = {
            "hide-on-ok" = false;
            "format" = "systemd: 󰀩 {nr_failed}";
            "format-ok" = "systemd:  ";
            "system" = true;
            "user" = true;
          };
        };
      };
      style =
        # css
        ''
          * {
            border: none;
            border-radius: 0;
            font-family: JetBrainsMono Nerd Font, monospace;
            font-size: 18;
            font-weight: 500;
            min-height: 0;
          }
          window#waybar.background {
            /* transparent background */
            background: rgba(0, 0, 0, 0);
          }
          #workspaces {
            padding: 0px 0px 0px 0px;
          }
          #workspaces button {
            color: ${foreground};
            background-color: ${bg3};
            min-width: 25px;
          }
          #workspaces button.empty {
            color: ${grey1};
            background-color: ${bg3};
          }
          /* for hyprland its active instead of focused*/
          #workspaces button.active {
            color: ${bg0};
            background-color: ${primary};
          }
          #workspaces button.focused {
            background-color: ${secondary};
            color: ${bg0};
          }
          #workspaces button.urgent {
            background-color: ${red};
            color: ${bg0};
          }
          #workspaces button:hover {
            transition-duration: 0s;
            box-shadow: inherit;
            text-shadow: inherit;
            background: ${secondary};
            color: ${bg0};
          }
          #scratchpad {
            background-color: ${blue};
            color: ${bg0};
            padding: 2px 5px;
          }
          #mode {
            padding-left: 10px;
            padding-right: 10px;
            background-color: ${green};
            color: ${bg3};
          }
          #submap {
            padding-left: 10px;
            padding-right: 10px;
            background-color: ${red};
            color: ${bg3};
          }
          #custom-mpd {
            padding-right: 10px;
            padding-left: 10px;
            color: ${foreground};
            background-color: ${bg3};
          }
          widget > * {
              padding-top: 2px;
              padding-bottom: 2px;
          }
          .modules-left > widget:first-child > * {
            margin-left: 0px;
          }
          .modules-left > widget:last-child > * {
            margin-right: 18px;
          }
          .modules-right > widget > * {
            padding: 0 12px;
            margin-left: 0;
            margin-right: 0;
            color: ${bg0};
            background-color: ${primary};
          }
          window#waybar.topBar .modules-center {
            padding: 0 12px;
            color: ${foreground};
            background-color: ${bg3};
          }
          @keyframes blink {
            to {
              color: ${green};
            }
          }
          #tray {
            background: ${bg3};
          }
          #custom-inhibitidle.active {
            color: ${bg0};
            background-color: ${red};
          }
          #custom-system-mode.normal {
            color: ${bg0};
            background-color: ${orange};
            padding: 0 12px;
          }
          #custom-system-mode.offline-focus {
            color: ${bg0};
            background-color: ${blue};
            padding: 0 12px;
          }
          label:focus {
            background-color: ${bg0};
          }
          tooltip {
            background: ${bg0};
          }
          tooltip label {
            color: ${green};
            text-shadow: none;
          }
          #systemd-failed-units.ok {
            background-color: ${green};
            color: ${bg0};
          }
          #systemd-failed-units.degraded {
            background-color: ${red};
            color: ${bg0};
          }
          #power-profiles-daemon {
            padding-left: 10px;
            padding-right: 10px;
          }
          #custom-vpn-status {
            padding-left: 12px;
            padding-right: 12px;
            min-width: 20px;
            font-family: "Nerd Font", monospace;
          }
          #custom-vpn-status.connected {
            color: ${bg0};
            background-color: ${blue};
          }
          #custom-vpn-status.disconnected {
            color: ${bg0};
            background-color: ${grey0};
          }
          #custom-vpn-status.error {
            color: ${bg0};
            background-color: ${red};
          }
        '';
    };
  };
}
