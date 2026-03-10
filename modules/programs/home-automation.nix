{
  config,
  pkgs,
  lib,
  ...
}:
let
  isLinux = pkgs.stdenv.isLinux;
  isDarwin = pkgs.stdenv.isDarwin;
  username = config.nx.username;
in
{
  options = {
    nx.programs.homeAutomation.enable = lib.mkEnableOption "enables home automation scripts etc" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.homeAutomation.enable (
    lib.mkMerge [
      # Linux: system-level sops
      (lib.mkIf isLinux {
        # home-assistant API key
        sops.secrets.hass_api_key = {
          owner = username;
          mode = "0600";
          sopsFile = ./secrets/home-assistant.sops.yaml;
        };
        # secret domain
        sops.secrets.hass_domain = {
          owner = username;
          mode = "0600";
          sopsFile = ./secrets/home-assistant.sops.yaml;
        };
        # add api key to environment
        environment.sessionVariables = {
          HASS_API_KEY = "$(cat ${config.sops.secrets.hass_api_key.path})";
          HASS_DOMAIN = "$(cat ${config.sops.secrets.hass_domain.path})";
        };
      })

      # Darwin: home-manager sops
      (lib.mkIf isDarwin {
        home-manager.users.${username} = {
          sops.secrets = {
            hass_api_key = {
              sopsFile = ./secrets/home-assistant.sops.yaml;
            };
            hass_domain = {
              sopsFile = ./secrets/home-assistant.sops.yaml;
            };
          };
          # add api key to environment
          home.sessionVariables = {
            HASS_API_KEY = "$(cat ${config.home-manager.users.${username}.sops.secrets.hass_api_key.path})";
            HASS_DOMAIN = "$(cat ${config.home-manager.users.${username}.sops.secrets.hass_domain.path})";
          };
        };
      })

      # Common configuration (both platforms)
      {
        home-manager.users.${username} = {
          # my scripts relevant to homeAutomation
          home.sessionPath = [ "$HOME/.local/scripts" ];

          # This script returns the current humidity in my office via home assistant
          home.file.".local/scripts/cli.home.office.getHumidity" = {
            executable = true;
            text = ''
              #!/bin/sh
              ${pkgs.curl}/bin/curl -X GET \
                -H "Authorization: Bearer ''${HASS_API_KEY}" \
                -H "Content-Type: application/json" \
                -s \
                https://''${HASS_DOMAIN}/api/states/sensor.office_sensor_humidity \
                | ${pkgs.jq}/bin/jq -r '. | "\(.state)\(.attributes.unit_of_measurement)"'
            '';
          };
          # This script returns the current temperature in my office via home assistant
          home.file.".local/scripts/cli.home.office.getTemperature" = {
            executable = true;
            text = ''
              #!/bin/sh
              ${pkgs.curl}/bin/curl -X GET \
                -H "Authorization: Bearer ''${HASS_API_KEY}" \
                -H "Content-Type: application/json" \
                -s \
                https://''${HASS_DOMAIN}/api/states/sensor.office_sensor_temperature \
                | ${pkgs.jq}/bin/jq -r '. | "\(.state)\(.attributes.unit_of_measurement)"'
            '';
          };
          # This script opens the blinds in the office
          home.file.".local/scripts/home.office.openBlinds" = {
            executable = true;
            text = ''
              #!/bin/sh
              ${pkgs.curl}/bin/curl -X POST \
                -H "Authorization: Bearer ''${HASS_API_KEY}" \
                -H "Content-Type: application/json" \
                -d '{"entity_id": "cover.office_blinds"}' \
                https://''${HASS_DOMAIN}/api/services/cover/open_cover
            '';
          };
          # This script closes the blinds in the office
          home.file.".local/scripts/home.office.closeBlinds" = {
            executable = true;
            text = ''
              #!/bin/sh
              ${pkgs.curl}/bin/curl -X POST \
                -H "Authorization: Bearer ''${HASS_API_KEY}" \
                -H "Content-Type: application/json" \
                -d '{"entity_id": "cover.office_blinds"}' \
                https://''${HASS_DOMAIN}/api/services/cover/close_cover
            '';
          };
          # This script closes only the LEFT blind in the office
          home.file.".local/scripts/home.office.closeBlindsLeft" = {
            executable = true;
            text = ''
              #!/bin/sh
              ${pkgs.curl}/bin/curl -X POST \
                -H "Authorization: Bearer ''${HASS_API_KEY}" \
                -H "Content-Type: application/json" \
                -d '{"entity_id": "cover.office_left"}' \
                https://''${HASS_DOMAIN}/api/services/cover/close_cover
            '';
          };
          # This script closes only the RIGHT blind in the office
          home.file.".local/scripts/home.office.closeBlindsRight" = {
            executable = true;
            text = ''
              #!/bin/sh
              ${pkgs.curl}/bin/curl -X POST \
                -H "Authorization: Bearer ''${HASS_API_KEY}" \
                -H "Content-Type: application/json" \
                -d '{"entity_id": "cover.office_right"}' \
                https://''${HASS_DOMAIN}/api/services/cover/close_cover
            '';
          };
          # Turns on the heater in the office
          home.file.".local/scripts/home.office.heaterOn" = {
            executable = true;
            text = ''
              #!/bin/sh
              ${pkgs.curl}/bin/curl -X POST \
                -H "Authorization: Bearer ''${HASS_API_KEY}" \
                -H "Content-Type: application/json" \
                -d '{"entity_id": "switch.office_heater"}' \
                https://''${HASS_DOMAIN}/api/services/switch/turn_on
            '';
          };
          # Turns off the heater in the office
          home.file.".local/scripts/home.office.heaterOff" = {
            executable = true;
            text = ''
              #!/bin/sh
              ${pkgs.curl}/bin/curl -X POST \
                -H "Authorization: Bearer ''${HASS_API_KEY}" \
                -H "Content-Type: application/json" \
                -d '{"entity_id": "switch.office_heater"}' \
                https://''${HASS_DOMAIN}/api/services/switch/turn_off
            '';
          };
          # relevant home automation keyboard shortcuts in sway
          wayland.windowManager.sway.config =
            lib.mkIf config.home-manager.users.${username}.wayland.windowManager.sway.enable
              {
                keybindings =
                  let
                    super = "Mod4";
                    alt = "Mod1";
                    pageup = "Prior";
                    pagedown = "Next";
                    newwindow = config.nx.programs.defaultWebBrowserSettings.newWindowCmd;
                    homeDir = config.home-manager.users.${username}.home.homeDirectory;
                  in
                  {
                    "${super}+${pageup}" = "exec ${homeDir}/.local/scripts/home.office.openBlinds";
                    "${super}+${pagedown}" = "exec ${homeDir}/.local/scripts/home.office.closeBlinds";
                    "${alt}+h" = "exec ${newwindow} https://$HASS_DOMAIN";
                  };
              };
          wayland.windowManager.hyprland.settings =
            lib.mkIf config.home-manager.users.${username}.wayland.windowManager.hyprland.enable
              {
                bind =
                  let
                    pageup = "Prior";
                    pagedown = "Next";
                    homeDir = config.home-manager.users.${username}.home.homeDirectory;
                    newwindow = config.nx.programs.defaultWebBrowserSettings.newWindowCmd;
                  in
                  [
                    "SUPER, ${pageup}, exec, ${homeDir}/.local/scripts/home.office.openBlinds"
                    "SUPER, ${pagedown}, exec, ${homeDir}/.local/scripts/home.office.closeBlinds"
                    "ALT, h, exec, ${newwindow} https://$HASS_DOMAIN"
                  ];
              };
          # This script turns off the grow lights
          home.file.".local/scripts/home.office.turnGrowlightsOff" = {
            executable = true;
            text = ''
              #!/bin/sh
              ${pkgs.curl}/bin/curl -X POST \
                -H "Authorization: Bearer ''${HASS_API_KEY}" \
                -H "Content-Type: application/json" \
                -d '{"entity_id": "automation.growlight_off"}' \
                https://''${HASS_DOMAIN}/api/services/automation/trigger
            '';
          };
          # This script turns on the grow lights
          home.file.".local/scripts/home.office.turnGrowlightsOn" = {
            executable = true;
            text = ''
              #!/bin/sh
              ${pkgs.curl}/bin/curl -X POST \
                -H "Authorization: Bearer ''${HASS_API_KEY}" \
                -H "Content-Type: application/json" \
                -d '{"entity_id": "automation.growlight_timer"}' \
                https://''${HASS_DOMAIN}/api/services/automation/trigger
            '';
          };
        };
      }
    ]
  );
}
