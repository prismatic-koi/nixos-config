{
  config,
  lib,
  pkgs,
  ...
}:
let
  homeDir = config.home-manager.users.${config.nx.username}.home.homeDirectory;
  theme = config.theme;
  inherit (lib.generators) mkLuaInline;
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
    };
  };
  config = lib.mkIf pkgs.stdenv.isLinux (
    lib.mkMerge [
      {
        # enable for system
        programs.hyprland.enable = true;
        # configure for user
        home-manager.users.${config.nx.username}.wayland.windowManager.hyprland =
          let
            scriptsDir = "${homeDir}/.local/scripts";
            # scripts
            emojipicker = "${scriptsDir}/application.rofi.emojipicker";
            runscripts = "${scriptsDir}/application.scripts.launcher";
            calculator = "${scriptsDir}/application.rofi.calculator";
            applicationlauncher = "${scriptsDir}/application.launcher";
            toggleTouchpad = "${scriptsDir}/system.inputs.toggleTouchpad";
            volumeUp = "${scriptsDir}/system.audio.volumeUp";
            volumeDown = "${scriptsDir}/system.audio.volumeDown";
            toggleMute = "${scriptsDir}/system.audio.toggleMute";
            brightnessUp = "${scriptsDir}/system.display.brightnessUp";
            brightnessDown = "${scriptsDir}/system.display.brightnessDown";
            prismLauncher = "prism launch";
            # applications
            terminal = "kitty";
            browser = config.nx.programs.defaultWebBrowserSettings.cmd;
            newwindow = config.nx.programs.defaultWebBrowserSettings.newWindowCmd;
            calendar = "${newwindow} https://calendar.google.com";
            filemanager = "${terminal} lf";
            musicplayer = "${terminal} ncmpcpp";
            addtodailytodo = "${scriptsDir}/obsidian.dailyTodo.addItem";
            addtoshoppinglist = "${scriptsDir}/home.shoppinglist.addItem";
            openshoppinglist = "${newwindow} https://www.notion.so/ph3nx/Shopping-List-92d98ac3dc86460285a399c0b1176fc5";
            # configuration
            enableAudioControls = config.nx.externalAudio.enable == false;

            # Helpers for translating hyprlang-style binds into the lua
            # `hl.bind(...)` call shape that HM's lua renderer expects.
            #
            # `mkBind keys dispatcher` produces `hl.bind(keys, dispatcher)`.
            # `mkBindFlags keys dispatcher flags` adds a flag table as the
            # third arg (e.g. `{ locked = true; }`, `{ release = true;
            # transparent = true; }`).
            #
            # `dispatcher` is a string of raw lua (e.g.
            # `''hl.dsp.exec_cmd("kitty")''`) that we wrap in mkLuaInline so
            # the renderer emits it verbatim rather than quoting it.
            mkBind = keys: dispatcher: {
              _args = [
                keys
                (mkLuaInline dispatcher)
              ];
            };
            mkBindFlags = keys: dispatcher: flags: {
              _args = [
                keys
                (mkLuaInline dispatcher)
                flags
              ];
            };
            # `mkExec keys cmd` is shorthand for the (very common) bind that
            # exec's a shell command. `cmd` is passed through
            # `lib.generators.toLua` so any special characters in the string
            # are escaped correctly into the lua source.
            mkExec = keys: cmd: mkBind keys "hl.dsp.exec_cmd(${lib.generators.toLua { } cmd})";
            mkExecFlags =
              keys: cmd: flags:
              mkBindFlags keys "hl.dsp.exec_cmd(${lib.generators.toLua { } cmd})" flags;
          in
          {
            enable = true;
            # Lua renderer (Hyprland's preferred config language since 0.55).
            # See docs/hyprland-lua-migration.md for the design.
            configType = "lua";
            settings = {
              # ---- general ----------------------------------------------------
              general = {
                gaps_in = 5;
                gaps_out = 5;
                border_size = 3;
                # Gradient borders use the structured table form under lua
                # (`{ colors = { ... }, angle = 45 }`); the legacy
                # whitespace-separated string + "45deg" suffix isn't accepted
                # by the lua `hl.config` API — the upstream example config
                # ships exclusively in the table form (see
                # /share/hypr/hyprland.lua bundled with hyprland).
                col = {
                  active_border =
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
                      toRgba = color: "rgba(${builtins.substring 1 6 color}ff)";
                    in
                    {
                      colors = map toRgba colors;
                      angle = 45;
                    };
                  inactive_border = "rgba(${builtins.substring 1 6 (theme.bg2)}ff)";
                };
                layout = "dwindle";
              };
              # ---- input -----------------------------------------------------
              input = {
                # Te Reo Macrons
                kb_layout = "nz";
                kb_variant = "mao";
                kb_options = "lv3:rwin_switch";
                # keyrepeat settings (lua wants ints, not strings)
                repeat_delay = 225;
                repeat_rate = 60;
                follow_mouse = 2;
                sensitivity = -0.8;
                touchpad = {
                  # feels right for a touchpad
                  natural_scroll = true;
                };
              };
              # ---- decoration ------------------------------------------------
              decoration = {
                rounding = 5;
                blur.enabled = true;
                shadow = {
                  enabled = false;
                };
              };
              # ---- dwindle ---------------------------------------------------
              dwindle = {
                # lua wants bool, not "yes"/"no"
                preserve_split = true;
                force_split = 2;
              };
              # ---- misc ------------------------------------------------------
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
              # ---- cursor ----------------------------------------------------
              cursor = {
                inactive_timeout = 5;
              };
              # ---- debug -----------------------------------------------------
              debug = {
                disable_logs = false;
              };
              # ---- ecosystem -------------------------------------------------
              ecosystem = {
                # don't show update notifications each boot
                no_update_news = true;
              };
              # ---- curves (bezier renamed in lua) ----------------------------
              # `curve` is in importantPrefixes so renders before `animation`.
              curve = [
                {
                  _args = [
                    "myBezier"
                    {
                      type = "bezier";
                      points = [
                        [
                          0.05
                          0.9
                        ]
                        [
                          0.1
                          1.0
                        ]
                      ];
                    }
                  ];
                }
                {
                  _args = [
                    "linear"
                    {
                      type = "bezier";
                      points = [
                        [
                          0
                          0
                        ]
                        [
                          1
                          1
                        ]
                      ];
                    }
                  ];
                }
              ];
              # ---- animations ------------------------------------------------
              # master switch
              animations = {
                enabled = true;
              };
              # per-leaf animation definitions; top-level list (not nested
              # under `animations`), one `hl.animation(...)` call per entry.
              # The curve-reference field is named `bezier` (matching the
              # upstream-shipped example at /share/hypr/hyprland.lua), not
              # `curve` — different curve types use different field names
              # (`bezier = "..."` vs `spring = "..."`).
              animation = [
                {
                  leaf = "windows";
                  enabled = true;
                  speed = 3;
                  bezier = "myBezier";
                }
                {
                  leaf = "windowsOut";
                  enabled = true;
                  speed = 2;
                  bezier = "myBezier";
                  style = "popin 90%";
                }
                {
                  leaf = "windowsIn";
                  enabled = true;
                  speed = 2;
                  bezier = "myBezier";
                  style = "popin 90%";
                }
                {
                  leaf = "border";
                  enabled = true;
                  speed = 2;
                  # hyprlang implicitly provided a `default` bezier; the lua
                  # API doesn't — referencing an undefined curve here drops
                  # Hyprland into a safe state with no keybinds at startup.
                  # Fall back to the `linear` curve we define explicitly.
                  bezier = "linear";
                }
                {
                  leaf = "borderangle";
                  enabled = true;
                  speed = 50;
                  bezier = "linear";
                  style = "loop";
                }
                {
                  leaf = "fade";
                  enabled = true;
                  speed = 2;
                  bezier = "linear";
                }
                {
                  leaf = "workspaces";
                  enabled = true;
                  speed = 1;
                  bezier = "myBezier";
                  style = "slidevert";
                }
              ];
              # ---- monitor ---------------------------------------------------
              monitor = [
                {
                  output = "";
                  mode = "preferred";
                  position = "auto";
                  scale = 1;
                }
              ];
              # ---- device ----------------------------------------------------
              device = [
                {
                  # reduce touchpad sensitivity
                  name = "asup1415:00-093a:300c-touchpad";
                  sensitivity = 0.5;
                }
              ];
              # ---- env -------------------------------------------------------
              env = [
                {
                  _args = [
                    "XDG_CURRENT_DESKTOP"
                    "Hyprland"
                  ];
                }
                {
                  _args = [
                    "XDG_SESSION_TYPE"
                    "wayland"
                  ];
                }
                {
                  _args = [
                    "XDG_SESSION_DESKTOP"
                    "Hyprland"
                  ];
                }
                {
                  _args = [
                    "GDK_BACKEND"
                    "wayland,x11"
                  ];
                }
                # "SDL_VIDEODRIVER,wayland" # removed: causes stutter in Proton games, let Steam/Proton pick the backend
                {
                  _args = [
                    "_JAVA_AWT_WM_NONREPARENTING"
                    "1"
                  ];
                }
                {
                  _args = [
                    "QT_QPA_PLATFORM"
                    "wayland"
                  ];
                }
              ];
              # ---- startup hook ----------------------------------------------
              # Replaces hyprlang `exec-once` and `exec`. `hl.on("hyprland.start", ...)`
              # is the lua-idiomatic equivalent: run these commands once at
              # session start. We fold both lists into one hook (`exec`'s
              # per-reload semantics aren't relied on by anything here — both
              # entries are idempotent restart-style commands).
              #
              # `lib.mkIf <false> { ... }` entries are stripped by the module
              # system before they reach the renderer, so the function body
              # only contains the actually-active commands per machine.
              # `lib.optional <bool> <x>` evaluates to `[ <x> ]` or `[]` —
              # cleaner than `lib.mkIf` here because we're building a plain
              # list in let-scope, not contributing to an option, so the
              # module system's `mkIf`-stripping pass never runs.
              on =
                let
                  startupCmds =
                    lib.optional config.nx.desktop.wallpaper.enable "swaybg -i ${homeDir}/.config/wallpaper-${config.nx.desktop.wallpaper.resolution}.png --mode fill"
                    ++ [
                      "hypridle"
                    ]
                    ++ lib.optional (config.nx.isLaptop == false) "steam -silent -no-cef-sandbox"
                    ++ [
                      "${scriptsDir}/game.inputRemapper.defaults"
                    ]
                    # default to 70% brightness on laptops
                    ++ lib.optional config.nx.isLaptop "${pkgs.brightnessctl}/bin/brightnessctl s 70%"
                    # default to keyboard backlight off on laptops
                    ++ lib.optional config.nx.isLaptop "${pkgs.brightnessctl}/bin/brightnessctl --device='asus::kbd_backlight' set 0"
                    ++ [
                      "${scriptsDir}/cli.hyprland.switchWorkspaceOnWindowClose"
                      "waybar"
                      # ex-`exec` entries (now also once-per-session):
                      "pkill waybar && hyprctl dispatch exec waybar"
                      "${scriptsDir}/cli.system.setHyprGaps"
                    ];
                in
                {
                  _args = [
                    "hyprland.start"
                    (mkLuaInline ''
                      function()
                      ${lib.concatMapStringsSep "\n" (cmd: "  hl.exec_cmd(${lib.generators.toLua { } cmd})") startupCmds}
                      end
                    '')
                  ];
                };
              # ---- window rules (lua name: window_rule, underscore) ----------
              # The main module contributes nothing; per-app rules merge in
              # from gaming/, programs/qutebrowser/, etc. via list-merge.
              window_rule = [ ];
              # ---- binds -----------------------------------------------------
              # In lua, `bind` / `bindl` / `bindrt` / `bindm` collapse into a
              # single `hl.bind(keys, dispatcher, flags?)` call; flag tables
              # encode the variant (`{ locked = true; }`, etc.).
              # Key strings use the lua " + "-joined form (e.g. "SUPER + h"),
              # not the hyprlang "MOD, KEY" comma form. No-mod binds drop the
              # leading comma entirely (e.g. "XF86AudioPlay", not ",
              # XF86AudioPlay"). The format matches the canonical example
              # config shipped at /share/hypr/hyprland.lua.
              bind = [
                # show waybar + quickshell widgets on SUPER_L keydown
                (mkExec "SUPER_L" "pkill -SIGUSR1 waybar; hyprctl dispatch event quickshell:show")
                # hide waybar + quickshell widgets on SUPER_L keyup. Single
                # top-level entry with `submap_universal = true` replaces the
                # previous three copies (top-level + inside `resize` + inside
                # `exit` submap) — see docs section 8.5.
                (mkBindFlags "SUPER_L"
                  ''hl.dsp.exec_cmd("pkill -SIGUSR2 waybar; hyprctl dispatch event quickshell:hide")''
                  {
                    release = true;
                    transparent = true;
                    submap_universal = true;
                  }
                )
                # Motions
                # focus window
                (mkBind "SUPER + h" ''hl.dsp.focus({ direction = "l" })'')
                (mkBind "SUPER + j" ''hl.dsp.focus({ direction = "d" })'')
                (mkBind "SUPER + k" ''hl.dsp.focus({ direction = "u" })'')
                (mkBind "SUPER + l" ''hl.dsp.focus({ direction = "r" })'')
                # move window
                (mkBind "SUPER + SHIFT + H" ''hl.dsp.window.move({ direction = "l" })'')
                (mkBind "SUPER + SHIFT + J" ''hl.dsp.window.move({ direction = "d" })'')
                (mkBind "SUPER + SHIFT + K" ''hl.dsp.window.move({ direction = "u" })'')
                (mkBind "SUPER + SHIFT + L" ''hl.dsp.window.move({ direction = "r" })'')
                # switch workspace
                (mkBind "SUPER + 1" "hl.dsp.workspace(1)")
                (mkBind "SUPER + 2" "hl.dsp.workspace(2)")
                (mkBind "SUPER + 3" "hl.dsp.workspace(3)")
                (mkBind "SUPER + 4" "hl.dsp.workspace(4)")
                (mkBind "SUPER + 5" "hl.dsp.workspace(5)")
                (mkBind "SUPER + 6" "hl.dsp.workspace(6)")
                (mkBind "SUPER + 7" "hl.dsp.workspace(7)")
                (mkBind "SUPER + 8" "hl.dsp.workspace(8)")
                (mkBind "SUPER + 9" "hl.dsp.workspace(9)")
                (mkBind "SUPER + TAB" ''hl.dsp.workspace("previous")'')
                # move active window to workspace (silent — no follow)
                (mkBind "SUPER + SHIFT + 1" "hl.dsp.window.move({ workspace = 1, silent = true })")
                (mkBind "SUPER + SHIFT + 2" "hl.dsp.window.move({ workspace = 2, silent = true })")
                (mkBind "SUPER + SHIFT + 3" "hl.dsp.window.move({ workspace = 3, silent = true })")
                (mkBind "SUPER + SHIFT + 4" "hl.dsp.window.move({ workspace = 4, silent = true })")
                (mkBind "SUPER + SHIFT + 5" "hl.dsp.window.move({ workspace = 5, silent = true })")
                (mkBind "SUPER + SHIFT + 6" "hl.dsp.window.move({ workspace = 6, silent = true })")
                (mkBind "SUPER + SHIFT + 7" "hl.dsp.window.move({ workspace = 7, silent = true })")
                (mkBind "SUPER + SHIFT + 8" "hl.dsp.window.move({ workspace = 8, silent = true })")
                (mkBind "SUPER + SHIFT + 9" "hl.dsp.window.move({ workspace = 9, silent = true })")
                # floating
                (mkBind "SUPER + SHIFT + space" ''hl.dsp.window.float({ action = "toggle" })'')
                # special workspace
                (mkBind "SUPER + X" ''hl.dsp.workspace.toggle_special("magic")'')
                (mkBind "SUPER + SHIFT + X" ''hl.dsp.window.move({ workspace = "special:magic", silent = true })'')
                # scroll through existing workspaces
                (mkBind "SUPER + mouse_down" ''hl.dsp.workspace("e+1")'')
                (mkBind "SUPER + mouse_up" ''hl.dsp.workspace("e-1")'')
                # window shortcuts
                (mkBind "SUPER + q" "hl.dsp.window.close()")
                (mkExec "SUPER + SHIFT + C" "hyprctl reload")
                (mkExec "SUPER + period" emojipicker)
                (mkExec "SUPER + Space" runscripts)
                (mkExec "SUPER + c" calculator)
                (mkBind "SUPER + SHIFT + F" "hl.dsp.window.fullscreen()")
                (mkExec "SUPER + i" "${scriptsDir}/cli.system.inhibitIdle toggle")
                # Notification Center
                (mkExec "SUPER + n" "${pkgs.swaynotificationcenter}/bin/swaync-client -t -sw")
                (mkExec "SUPER + SHIFT + N" "${pkgs.swaynotificationcenter}/bin/swaync-client --close-all && ${pkgs.swaynotificationcenter}/bin/swaync-client --close-panel")
                # application shortcuts
                (mkExec "ALT + Return" terminal)
                (mkExec "ALT + Space" applicationlauncher)
                (mkExec "ALT + a" "anki")
                (mkExec "ALT + b" browser)
                (mkExec "ALT + c" calendar)
                (mkExec "ALT + f" filemanager)
                (mkExec "ALT + m" musicplayer)
                (mkExec "ALT + t" addtodailytodo)
                (mkExec "ALT + l" addtoshoppinglist)
                (mkExec "ALT + SHIFT + l" openshoppinglist)
                (mkExec "ALT + o" "${prismLauncher} --path ${homeDir}/documents/obsidian")
                (mkExec "ALT + n" "${prismLauncher} --path ${homeDir}/code/nixos-config/main")
                (mkExec "ALT + p" prismLauncher)
                # submap entries — schedule auto-reset via hl.timer (ms) at
                # the same time as we dispatch the submap, removing the need
                # for the old `sleep N && hyprctl dispatch submap reset`
                # shell-out. `hl.bind` accepts either an `hl.dsp.*` dispatcher
                # value or a plain lua function as its second argument; we
                # use a function here so the timer is scheduled in the same
                # call as the submap entry.
                (mkBind "SUPER + R" ''
                  function()
                    hl.dispatch(hl.dsp.submap("resize"))
                    hl.timer(function()
                      hl.dispatch(hl.dsp.submap("reset"))
                    end, { timeout = 10000, type = "oneshot" })
                  end
                '')
                (mkBind "SUPER + SHIFT + E" ''
                  function()
                    hl.dispatch(hl.dsp.submap("exit"))
                    hl.timer(function()
                      hl.dispatch(hl.dsp.submap("reset"))
                    end, { timeout = 3000, type = "oneshot" })
                  end
                '')
                # media controls
                (mkExec "XF86AudioMute" toggleMute)
                (lib.mkIf enableAudioControls (mkExec "XF86AudioRaiseVolume" volumeUp))
                (lib.mkIf enableAudioControls (mkExec "XF86AudioLowerVolume" volumeDown))
                (mkExec "XF86AudioPlay" "${pkgs.playerctl}/bin/playerctl play-pause")
                (mkExec "Pause" "${pkgs.playerctl}/bin/playerctl play-pause")
                # fn+k on my asus laptop
                (mkExec "Scroll_Lock" "${pkgs.playerctl}/bin/playerctl stop")
                (mkExec "XF86AudioStop" "${pkgs.playerctl}/bin/playerctl stop")
                (mkExec "XF86AudioNext" "${pkgs.playerctl}/bin/playerctl next")
                (mkExec "XF86AudioPrev" "${pkgs.playerctl}/bin/playerctl previous")
                (lib.mkIf config.nx.isLaptop (mkExec "XF86MonBrightnessUp" brightnessUp))
                (lib.mkIf config.nx.isLaptop (mkExec "XF86MonBrightnessDown" brightnessDown))
                # The Asus laptop firmware maps the touchpad toggle Fn key to Super+P.
                (lib.mkIf config.nx.isLaptop (mkExec "SUPER + p" toggleTouchpad))
                # print screen
                (mkExec "Print" "${scriptsDir}/application.grim.fullScreenshotToFile")
                # mouse (formerly bindm). `mouse = true` flag matches the
                # upstream-shipped example at /share/hypr/hyprland.lua.
                (mkBindFlags "SUPER + mouse:272" "hl.dsp.window.drag()" { mouse = true; })
                (mkBindFlags "SUPER + mouse:273" "hl.dsp.window.resize()" { mouse = true; })
                # locked (formerly bindl) — suspend works even when locked
                (mkBindFlags "SUPER + s" ''hl.dsp.exec_cmd("${scriptsDir}/cli.system.suspend")'' { locked = true; })
              ];
            };
            # ---- submaps -----------------------------------------------------
            # Migrated from extraConfig. `extraConfig` is now empty.
            #
            # The `SUPER_L` release-hide bind that used to be duplicated in
            # each submap is replaced by the single top-level entry above with
            # `{ submap_universal = true; release = true; transparent = true; }`
            # — see docs section 8.5.
            submaps = {
              resize = {
                settings.bind = [
                  (mkBindFlags "h" "hl.dsp.window.resize({ x = -10, y = 0, relative = true })" {
                    repeating = true;
                  })
                  (mkBindFlags "j" "hl.dsp.window.resize({ x = 0, y = 10, relative = true })" {
                    repeating = true;
                  })
                  (mkBindFlags "k" "hl.dsp.window.resize({ x = 0, y = -10, relative = true })" {
                    repeating = true;
                  })
                  (mkBindFlags "l" "hl.dsp.window.resize({ x = 10, y = 0, relative = true })" {
                    repeating = true;
                  })
                  (mkBind "escape" ''hl.dsp.submap("reset")'')
                ];
              };
              exit = {
                settings.bind = [
                  (mkExec "l" "hyprlock")
                  (mkExec "SHIFT + L" "loginctl terminate-user $USER")
                  (mkExec "s" "systemctl poweroff")
                  (mkExec "r" "systemctl reboot")
                  (mkBind "escape" ''hl.dsp.submap("reset")'')
                ];
              };
            };
            extraConfig = "";
            systemd = {
              enable = true;
            };
            xwayland.enable = true;
          };
      }
      {
        home-manager.users.${config.nx.username}.home.packages = with pkgs; [
          dex
          grim
          slurp
          swaybg
          wl-clipboard
          (lib.mkIf config.nx.isLaptop brightnessctl)
        ];
      }
    ]
  );
}
