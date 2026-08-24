{
  config,
  pkgs,
  lib,
  ...
}:
with config.theme;
# Gap mapping (issue #2814): v1 `grey0` (used here for subtle 1px borders) has
# no theme slot; it maps to neutrals.background_5, the structural grey nearest
# the foreground, matching the qutebrowser/greasemonkey precedent.
{
  options = {
    nx.desktop.swaync.enable = lib.mkEnableOption "enables swaync" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.desktop.swaync.enable && pkgs.stdenv.hostPlatform.isLinux) {
    home-manager.users.${config.nx.username}.home = {
      packages = [
        pkgs.libnotify
        pkgs.swaynotificationcenter
      ];
      file = {
        ".config/swaync/config.json".text =
          # json
          ''
            {
              "$schema": "/etc/xdg/swaync/configSchema.json",
              "positionX": "right",
              "positionY": "top",
              "control-center-margin-top": 0,
              "control-center-margin-bottom": 0,
              "control-center-margin-right": 0,
              "control-center-margin-left": 0,
              "timeout": 10,
              "timeout-low": 5,
              "timeout-critical": 0,
              "fit-to-screen": true,
              "control-center-width": 500,
              "notification-window-width": 500,
              "keyboard-shortcuts": true,
              "image-visibility": "always",
              "transition-time": 200,
              "hide-on-clear": false,
              "hide-on-action": true,
              "script-fail-notify": true,
              "scripts": {
                "example-script": {
                  "exec": "echo 'Do something...'",
                  "urgency": "Normal"
                }
              },
              "notification-visibility": {
                "example-name": {
                  "state": "muted",
                  "urgency": "Low",
                  "app-name": "Spotify"
                }
              }
            }
          '';
        ".config/swaync/style.css".text =
          # css
          ''
            * {
              font-family: Quicksand, sans-serif;
            }
            .notification-row {
              outline: none;
            }
            .notification-row:focus,
            .notification-row:hover {
              background: none;
              border-radius: 5px;
            }
            .notification {
              border-radius: 5px;
              margin: 6px 12px;
              box-shadow: none;
              padding: 0;
              color: ${neutrals.foreground};
              border: 1px solid ${neutrals.background_5};
            }
            .notification:hover {
              background: ${hues.green};
              color: ${neutrals.background_dim};
            }
            .notification-content {
              background: transparent;
              padding: 6px;
              border-radius: 5px;
            }
            .close-button {
              background: ${neutrals.background_0};
              color: ${neutrals.foreground};
              text-shadow: none;
              padding: 0;
              border-radius: 100%;
              margin-top: 10px;
              margin-right: 16px;
              box-shadow: none;
              border: none;
              min-width: 24px;
              min-height: 24px;
            }
            .close-button:hover {
              box-shadow: none;
              text-shadow: none;
              color: ${neutrals.background_dim};
              background: ${hues.green};
              transition: all 0.15s ease-in-out;
              border: none;
            }
            .notification-default-action,
            .notification-action {
              padding: 4px;
              margin: 0;
              box-shadow: none;
              background: ${neutrals.background_0};
              border: 0px;
              color: inherit;
            }
            .notification-default-action:hover,
            .notification-action:hover {
              background: ${hues.green};
              color: ${neutrals.background_dim};
            }

            .notification-default-action {
              border-radius: 5px;
            }

            /* When alternative actions are visible */
            /* .notification-default-action:not(:only-child) { */
            /*   border-bottom-left-radius: 0px; */
            /*   border-bottom-right-radius: 0px; */
            /* } */

            /* add bottom border radius to eliminate clipping */
            /* .notification-action:first-child { */
            /*   border-bottom-left-radius: 0px; */
            /* } */

            .notification-action:last-child {
              /* border-bottom-right-radius: 0px; */
              border-right: 1px solid ${neutrals.background_5};
            }

            .body-image {
              margin-top: 6px;
              background-color: white;
              border-radius: 0px;
            }
            .image {
              border-radius: 0px;
            }

            .summary {
              font-size: 16px;
              font-weight: bold;
              background: transparent;
              text-shadow: none;
              color: inherit;
            }

            .time {
              font-size: 16px;
              font-weight: bold;
              background: transparent;
              text-shadow: none;
              margin-right: 18px;
              color: inherit;
            }

            .body {
              font-size: 15px;
              font-weight: normal;
              background: transparent;
              text-shadow: none;
              color: inherit;
            }

            /* The "Notifications" and "Do Not Disturb" text widget */
            .top-action-title {
              text-shadow: none;
              color: inherit;
            }

            .control-center {
              background: ${neutrals.background_0};
            }

            .control-center-list {
              background: transparent;
            }

            .floating-notifications {
              background: transparent;
            }

            /* Window behind control center and on all other monitors */
            .blank-window {
              background: alpha(black, 0.25);
            }

            /*** Widgets ***/

            /* Title widget */
            .widget-title {
              margin: 8px;
              font-size: 1.5rem;
            }
            /* Clear All Button */
            .widget-title > button {
              font-size: initial;
              color: ${neutrals.foreground};
              text-shadow: none;
              background: ${neutrals.background_0};
              border: 1px solid ${neutrals.background_5};
              box-shadow: none;
              border-radius: 5px;
            }
            .widget-title > button:hover {
              color: ${neutrals.background_dim};
              background: ${hues.green};
            }

            /* DND widget */
            .widget-dnd {
              margin: 8px;
              font-size: 1.1rem;
            }
            .widget-dnd > switch {
              font-size: initial;
              /* border-radius: 10px; */
              background: ${neutrals.background_0};
              border: 1px solid ${neutrals.background_5};
              box-shadow: none;
            }
            .widget-dnd > switch:checked {
              background: ${hues.blue};
            }
            .widget-dnd > switch slider {
              background: ${hues.green};
              /* border-radius: 5px; */
            }

            /* Label widget */
            .widget-label {
              margin: 8px;
            }
            .widget-label > label {
              font-size: 1.1rem;
            }

            /* Mpris widget */
            .widget-mpris {
              /* The parent to all players */
            }
            .widget-mpris-player {
              padding: 8px;
              margin: 8px;
            }
            .widget-mpris-title {
              font-weight: bold;
              font-size: 1.25rem;
            }
            .widget-mpris-subtitle {
              font-size: 1.1rem;
            }
          '';
      };
    };
  };
}
