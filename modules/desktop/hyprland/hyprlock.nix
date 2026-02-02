{
  config,
  lib,
  pkgs,
  ...
}:
let
  homeDir = config.home-manager.users.ben.home.homeDirectory;
  theme = config.theme;
  resolution = config.nx.desktop.wallpaper.resolution;
in
{
  options = {
    nx.desktop.hyprlock.enable = lib.mkEnableOption "enables hyprlock" // {
      # default to enabled if hyprland is
      default = config.nx.desktop.hyprland.enable;
    };
    nx.desktop.hyprlock.oled = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Enable OLED specific settings";
    };
  };
  config = lib.mkIf (config.nx.desktop.hyprlock.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben.programs.hyprlock =
      let
        oled = config.nx.desktop.hyprlock.oled;
        # Convert hex color to rgba format (remove # and add ff for alpha)
        toRgba = color: "rgba(${builtins.substring 1 6 color}ff)";
      in
      {
        enable = true;
        settings = {
          general = {
            hide_cursor = true;
            no_fade_in = true;
            no_fade_out = true;
          };
          background = [
            {
              monitor = "";
              path = lib.mkIf (oled == false) "${homeDir}/.config/wallpaper-${resolution}.png";
              color = lib.mkIf (oled == true) "rgba(0,0,0,1)";
              blur_passes = 0;
            }
          ];
          label =
            [
              {
                monitor = "";
                text = ''cmd[update:1000] echo $(date +'%T')'';
                color = toRgba theme.foreground;
                font_size = 95;
                font_family = "JetBrainsMono Nerd Font";
                position = "0, 100";
                halign = "center";
                valign = "center";
              }
              {
                monitor = "";
                text = ''cmd[update:1000] echo $(date +'%F')'';
                color = toRgba theme.foreground;
                font_size = 22;
                font_family = "JetBrainsMono Nerd Font";
                position = "0, 0";
                halign = "center";
                valign = "center";
              }
            ]
            ++ lib.optionals config.nx.isLaptop [
              {
                monitor = "";
                text = ''cmd[update:5000] ${homeDir}/.local/scripts/cli.system.batteryStatus'';
                color = toRgba theme.foreground;
                font_size = 18;
                font_family = "JetBrainsMono Nerd Font";
                position = "0, -200";
                halign = "center";
                valign = "center";
              }
            ];
          input-field = {
            monitor = "";
            size = "300,50";
            outline_thickness = 3;
            dots_size = 0.2; # Scale of input-field height, 0.2 - 0.8
            dots_spacing = 0.35; # Scale of dots' absolute size, 0.0 - 1.0
            dots_center = false;
            outer_color = toRgba theme.bg1;
            inner_color = toRgba theme.bg_dim;
            font_color = toRgba theme.green;
            fade_on_empty = false;
            rounding = 5;
            check_color = toRgba theme.bg_dim;
            fail_color = toRgba theme.red;
            placeholder_text = ''<i><span foreground="#${theme.foreground}">Input Password...</span></i>'';
            hide_input = false;
            position = "0, -100";
            halign = "center";
            valign = "center";
          };
        };
      };
  };
}
