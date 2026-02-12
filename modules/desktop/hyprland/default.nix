{
  config,
  lib,
  pkgs,
  ...
}:
let
  homeDir = config.home-manager.users.ben.home.homeDirectory;
  theme = config.theme;
in
{
  imports = [
    ./idle.nix
    ./scripts.nix
    ./hyprlock.nix
  ];
  options = {
    nx.desktop.hyprland = {
      enable = lib.mkEnableOption "enable the Hyprland desktop module" // {
        default = true;
      };
      layout = lib.mkOption {
        type = lib.types.enum [
          "dwindle"
          "hy3"
        ];
        default = "dwindle";
        description = "sets the default layout for Hyprland";
      };
      disableWorkspaceAnimations = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "disables workspace animations";
      };
    };
  };
  config = lib.mkIf pkgs.stdenv.isLinux (
    let
      layout = config.nx.desktop.hyprland.layout;
    in
    {
      # enable for system
      programs.hyprland.enable = true;
      # configure for user
      home-manager.users.ben.wayland.windowManager.hyprland =
        let
          scriptsDir = "${homeDir}/.local/scripts";
          # scripts
          emojipicker = "${scriptsDir}/application.rofi.emojipicker";
          runscripts = "${scriptsDir}/application.scripts.launcher";
          calculator = "${scriptsDir}/application.rofi.calculator";
          nvimsessionlauncher = "${scriptsDir}/application.nvim.sessionLauncher";
          applicationlauncher = "${scriptsDir}/application.launcher";
          toggleTouchpad = "${scriptsDir}/system.inputs.toggleTouchpad";
          volumeUp = "${scriptsDir}/system.audio.volumeUp";
          volumeDown = "${scriptsDir}/system.audio.volumeDown";
          toggleMute = "${scriptsDir}/system.audio.toggleMute";
          brightnessUp = "${scriptsDir}/system.display.brightnessUp";
          brightnessDown = "${scriptsDir}/system.display.brightnessDown";
          prismLauncher = "${scriptsDir}/cli.prism.launch";
          # applications
          terminal = "kitty";
          browser = config.nx.programs.defaultWebBrowserSettings.cmd;
          newwindow = config.nx.programs.defaultWebBrowserSettings.newWindowCmd;
          calendar = "${newwindow} https://calendar.google.com";
          filemanager = "${terminal} lf";
          musicplayer = "${terminal} ncmpcpp";
          obsidian = "${terminal} ${scriptsDir}/cli.tmux.projectSessioniser ${homeDir}/documents/obsidian";
          nixosconfig = "${terminal} ${scriptsDir}/cli.tmux.projectSessioniser ${homeDir}/code/nixos-config";
          addtodailytodo = "${scriptsDir}/obsidian.dailyTodo.addItem";
          addtoshoppinglist = "${scriptsDir}/home.shoppinglist.addItem";
          openshoppinglist = "${newwindow} https://www.notion.so/ph3nx/Shopping-List-92d98ac3dc86460285a399c0b1176fc5";
          # configuration
          enableAudioControls = config.nx.externalAudio.enable == false;
        in
        {
          enable = true;
          settings = {
            exec-once =
              let
                resolution = config.nx.desktop.wallpaper.resolution;
              in
              [
                "${pkgs.swaynotificationcenter}/bin/swaync"
                (lib.mkIf config.nx.desktop.wallpaper.enable "swaybg -i ${homeDir}/.config/wallpaper-${resolution}.png --mode fill")
                # (lib.mkIf (config.theme.name == "everforest") "swaybg -i ${homeDir}/.config/wallpaper_everforest-${resolution}.png --mode fill")
                # (lib.mkIf (config.theme.name == "github-light") "swaybg -i ${homeDir}/.config/wallpaper_github_light-${resolution}.png --mode fill")
                # "swaybg --color ${builtins.substring 1 6 (theme.bg_dim)}"
                "hypridle"
                (lib.mkIf (config.nx.isLaptop == false) "steam -silent -no-cef-sandbox") # couldn't figure out xdg-autostart
                "${scriptsDir}/game.inputRemapper.defaults"
                # default to 70% brightness
                (lib.mkIf config.nx.isLaptop "${pkgs.brightnessctl}/bin/brightnessctl s 70%")
                # default to keyboard backlight off
                (lib.mkIf config.nx.isLaptop "${pkgs.brightnessctl}/bin/brightnessctl --device='asus::kbd_backlight' set 0")
                "${scriptsDir}/cli.hyprland.switchWorkspaceOnWindowClose"
                "waybar"
              ];
            debug = {
              disable_logs = false;
            };
            ecosystem = {
              # don't show update notifications each boot
              no_update_news = true;
            };
            exec = [
              # restart waybar, if for some reason it died
              "pkill waybar && hyprctl dispatch exec waybar"
              "${scriptsDir}/cli.system.setHyprGaps"
            ];
            input = {
              # Te Reo Macrons
              kb_layout = "nz";
              kb_variant = "mao";
              kb_options = "lv3:rwin_switch";
              # keyrepeat settings
              repeat_delay = "225";
              repeat_rate = "60";
              follow_mouse = 2;
              sensitivity = -0.5;
              touchpad = {
                # feels right for a touchpad
                natural_scroll = true;
              };
            };
            gestures = {
              # was removed in a recent version of hyprland, didn't use it much anyway
              # workspace_swipe = lib.mkIf config.nx.isLaptop true;
            };
            general = {
              gaps_in = 5;
              gaps_out = 5;
              border_size = 3;
              "col.active_border" =
                let
                  # rainbow border colors in order
                  colors = with theme; [
                    red
                    orange
                    yellow
                    green
                    aqua
                    blue
                    purple
                  ];
                  # convert hex colors to rgba format (remove # and add ff for alpha)
                  toRgba = color: "rgba(${builtins.substring 1 6 color}ff)";
                  rgbaColors = map toRgba colors;
                in
                "${lib.concatStringsSep " " rgbaColors} 45deg";
              "col.inactive_border" = "rgba(${builtins.substring 1 6 (theme.bg2)}ff)";
              layout = layout;
            };
            plugin = {
              hy3 = lib.mkIf (layout == "hy3") {
                autotile = {
                  enable = true;
                };
              };
            };
            cursor = {
              inactive_timeout = 5;
            };
            decoration = {
              rounding = 5;
              blur.enabled = true;
              shadow = {
                enabled = false;
              };
            };
            bezier = [
              "myBezier,0.05,0.9,0.1,1.0"
              "linear,0,0,1,1"
            ];
            animations = {
              enabled = true;
              animation = [
                "windows, 1, 2, myBezier"
                "windowsOut, 1, 1, myBezier, popin 90%"
                "windowsIn, 1, 1, myBezier, popin 90%"
                "border, 1, 2, default"
                "borderangle, 1, 50, linear, loop"
                "fade, 1, 2, default"
                (lib.mkIf (
                  config.nx.desktop.hyprland.disableWorkspaceAnimations != true
                ) "workspaces,1,1, myBezier")
                (lib.mkIf (config.nx.desktop.hyprland.disableWorkspaceAnimations == true) "workspaces,0")
              ];
            };
            dwindle = {
              pseudotile = "yes";
              preserve_split = "yes";
              force_split = 2;
            };
            misc = {
              disable_hyprland_logo = true;
              disable_splash_rendering = true;
              vrr = 1;
              # when opening another program from terminal, swallow the terminal
              enable_swallow = false;
              swallow_regex = "^(kitty|lf)$";
              swallow_exception_regex = "^(wev)$";
              # suppress start-hyprland warning when not using the watchdog wrapper
              disable_watchdog_warning = true;
            };
            device = [
              {
                # reduce touchpad sensitivity
                name = "asup1415:00-093a:300c-touchpad";
                sensitivity = 0.5;
              }
            ];
            # where possible, window rules should live with the app config
            windowrule = [ ];
            monitor = [
              ",preferred,auto,auto"
            ];
            bindrt = [
              # hide waybar on SUPER_L keyup (actually resets which loads it as hidden)
              "SUPER, SUPER_L, exec, pkill -SIGUSR2 waybar"
            ];
            bindl = [
              # suspend (works even when locked)
              "SUPER, s, exec, ${scriptsDir}/cli.system.suspend"
            ];
            bind = [
              # show waybar on SUPER_L keydown
              ", SUPER_L, exec, pkill -SIGUSR1 waybar"
              # Motions
              # focus window
              "SUPER, h, movefocus, l"
              "SUPER, j, movefocus, d"
              "SUPER, k, movefocus, u"
              "SUPER, l, movefocus, r"
              # move window
              "SUPER SHIFT, H, movewindow, l"
              "SUPER SHIFT, J, movewindow, d"
              "SUPER SHIFT, K, movewindow, u"
              "SUPER SHIFT, L, movewindow, r"
              # hy3
              (lib.mkIf (layout == "hy3") "SUPER SHIFT, B, exec, hyprctl dispatch hy3:makegroup h")
              (lib.mkIf (layout == "hy3") "SUPER SHIFT, V, exec, hyprctl dispatch hy3:makegroup v")
              # switch workspace
              "SUPER, 1, workspace, 1"
              "SUPER, 2, workspace, 2"
              "SUPER, 3, workspace, 3"
              "SUPER, 4, workspace, 4"
              "SUPER, 5, workspace, 5"
              "SUPER, 6, workspace, 6"
              "SUPER, 7, workspace, 7"
              "SUPER, 8, workspace, 8"
              "SUPER, 9, workspace, 9"
              "SUPER, TAB, workspace, previous"
              # move active window to workspace
              "SUPER SHIFT, 1, movetoworkspacesilent, 1"
              "SUPER SHIFT, 2, movetoworkspacesilent, 2"
              "SUPER SHIFT, 3, movetoworkspacesilent, 3"
              "SUPER SHIFT, 4, movetoworkspacesilent, 4"
              "SUPER SHIFT, 5, movetoworkspacesilent, 5"
              "SUPER SHIFT, 6, movetoworkspacesilent, 6"
              "SUPER SHIFT, 7, movetoworkspacesilent, 7"
              "SUPER SHIFT, 8, movetoworkspacesilent, 8"
              "SUPER SHIFT, 9, movetoworkspacesilent, 9"
              # floating
              "SUPER SHIFT, space, togglefloating"
              # example special workspace TODO more
              "SUPER, X, togglespecialworkspace, magic"
              "SUPER SHIFT, X, movetoworkspacesilent, special:magic"
              # scroll through existing workspaces
              "SUPER, mouse_down, workspace, e+1"
              "SUPER, mouse_up, workspace, e-1"
              # window shortcuts
              "SUPER, q, killactive"
              "SUPER SHIFT, C, exec, hyprctl reload"
              "SUPER, period, exec, ${emojipicker}"
              "SUPER, Space, exec, ${runscripts}"
              "SUPER, c, exec, ${calculator}"
              "SUPER, f, exec, ${nvimsessionlauncher}"
              "SUPER SHIFT, F, fullscreen"
              "SUPER, i, exec, ${scriptsDir}/cli.system.inhibitIdle toggle"
              # Notification Center
              "SUPER, n, exec, ${pkgs.swaynotificationcenter}/bin/swaync-client -t -sw"
              "SUPER SHIFT, N, exec, ${pkgs.swaynotificationcenter}/bin/swaync-client --close-all && ${pkgs.swaynotificationcenter}/bin/swaync-client --close-panel"
              # application shortcuts
              "ALT, Return, exec, ${terminal}"
              "AlT, Space, exec, ${applicationlauncher}"
              "ALT, a, exec, anki"
              "ALT, b, exec, ${browser}"
              "ALT, c, exec, ${calendar}"
              "ALT, f, exec, ${filemanager}"
              "ALT, m, exec, ${musicplayer}"
              "ALT, t, exec, ${addtodailytodo}"
              "ALT, l, exec, ${addtoshoppinglist}"
              "ALT SHIFT, l, exec, ${openshoppinglist}"
              "ALT, o, exec, ${obsidian}"
              "ALT, n, exec, ${nixosconfig}"
              "ALT, p, exec, ${prismLauncher}"
              # media controls
              ", XF86AudioMute, exec, ${toggleMute}"
              (lib.mkIf enableAudioControls ", XF86AudioRaiseVolume, exec, ${volumeUp}")
              (lib.mkIf enableAudioControls ", XF86AudioLowerVolume, exec, ${volumeDown}")
              ", XF86AudioPlay, exec, ${pkgs.playerctl}/bin/playerctl play-pause"
              ", Pause, exec, ${pkgs.playerctl}/bin/playerctl play-pause"
              ", Scroll_Lock, exec, ${pkgs.playerctl}/bin/playerctl stop" # this is fn+k on my asus laptop
              ", XF86AudioStop, exec, ${pkgs.playerctl}/bin/playerctl stop"
              ", XF86AudioNext, exec, ${pkgs.playerctl}/bin/playerctl next"
              ", XF86AudioPrev, exec, ${pkgs.playerctl}/bin/playerctl previous"
              (lib.mkIf config.nx.isLaptop ", XF86MonBrightnessUp, exec, ${brightnessUp}")
              (lib.mkIf config.nx.isLaptop ", XF86MonBrightnessDown, exec, ${brightnessDown}")
              # on my asus laptop, one of the function keys presses Super_L+p for some reason for touchpad disable
              (lib.mkIf config.nx.isLaptop "SUPER, p, exec, ${toggleTouchpad}")
              # print screen
              ", Print, exec, ${scriptsDir}/application.grim.fullScreenshotToFile"
            ];
            bindm = [
              "SUPER, mouse:272, movewindow"
              "SUPER, mouse:273, resizewindow"
            ];
            # bindl = [
            #   ", switch:on:Lid Switch, exec, ${scriptsDir}/cli.system.suspend"
            # ];
            env = [
              "XDG_CURRENT_DESKTOP,Hyprland"
              "XDG_SESSION_TYPE,wayland"
              "XDG_SESSION_DESKTOP,Hyprland"
              "GDK_BACKEND,wayland,x11"
              # "SDL_VIDEODRIVER,wayland" # removed: causes stutter in Proton games, let Steam/Proton pick the backend
              "_JAVA_AWT_WM_NONREPARENTING,1"
              "QT_QPA_PLATFORM,wayland"
            ];
          };
          extraConfig = ''
            # resize submap (with auto reset after 10 sec)
            bind=SUPER,R,exec,sleep 10 && hyprctl dispatch submap reset
            bind=SUPER,R,submap,resize
            submap=resize
            binde=,h,resizeactive,-10 0
            binde=,j,resizeactive,0 10
            binde=,k,resizeactive,0 -10
            binde=,l,resizeactive,10 0
            bind=,escape,submap,reset
            submap=reset
            # exit submap (with auto reset after 3 sec)
            bind=SUPER SHIFT,E,exec,sleep 3 && hyprctl dispatch submap reset
            bind=SUPER SHIFT,E,submap,exit
            submap=exit
            # lock
            binde=,l,exec, hyprlock
            # logout
            bind=SHIFT,L,exec, loginctl terminate-user $USER
            # shutdown
            binde=,s,exec, systemctl poweroff
            # reboot
            binde=,r,exec, systemctl reboot
            bind=,escape,submap,reset
            submap=reset
          '';
          systemd = {
            enable = true;
          };
          xwayland.enable = true;
          plugins = [
            (lib.mkIf (layout == "hy3") pkgs.hyprlandPlugins.hy3)
          ];
        };
      home-manager.users.ben.home.packages = with pkgs; [
        dex
        grim
        slurp
        swaybg
        wl-clipboard
        (lib.mkIf config.nx.isLaptop brightnessctl)
      ];
    }
  );
}
