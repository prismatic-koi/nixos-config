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
  config = lib.mkIf pkgs.stdenv.hostPlatform.isLinux (
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
              # ---- section settings via hl.config({...}) ---------------------
              # The lua API only exposes a discrete set of top-level functions
              # (`hl.bind`, `hl.monitor`, `hl.device`, `hl.curve`,
              # `hl.animation`, `hl.window_rule`, `hl.env`, `hl.on`, etc.).
              # "Section" settings — `general`, `input`, `decoration`,
              # `dwindle`, `misc`, `cursor`, `debug`, `ecosystem`,
              # `animations` (master switch), etc. — are NOT individual
              # functions; setting them at the top level of `settings` would
              # render as `hl.general(...)`, `hl.input(...)`, etc. and crash
              # Hyprland with "attempt to call a nil value" at startup.
              #
              # All section settings must instead be passed inside a single
              # `hl.config({...})` call. We emit that explicitly via the
              # `_args` shape. Reference: the HM lua-config test fixture at
              # nixpkgs/.../tests/modules/services/hyprland/lua-config.nix
              # nests every section under `settings.config = {...}`, and the
              # upstream-shipped example at /share/hypr/hyprland.lua does the
              # same.
              config = {
                _args = [
                  {
                    general = {
                      gaps_in = 5;
                      gaps_out = 5;
                      border_size = 3;
                      # Gradient borders use the structured table form under
                      # lua (`{ colors = { ... }, angle = 45 }`); the legacy
                      # whitespace-separated string + "45deg" suffix isn't
                      # accepted by the lua `hl.config` API — the upstream
                      # example ships exclusively in the table form.
                      col = {
                        active_border =
                          let
                            # rainbow border colors in order. v1 `aqua` has no
                            # theme slot; it maps to hues.teal — exact on
                            # edge/github-light/gruvbox/nightcity, close on
                            # latte/onedark (where hues.cyan is the exact match).
                            # teal chosen for majority-scheme parity and to keep
                            # firefox/qutebrowser consistent (issue #2814).
                            colors = [
                              theme.hues.red
                              theme.hues.orange
                              theme.hues.yellow
                              theme.hues.green
                              theme.hues.teal
                              theme.hues.blue
                              theme.hues.purple
                            ];
                            toRgba = color: "rgba(${builtins.substring 1 6 color}ff)";
                          in
                          {
                            colors = map toRgba colors;
                            angle = 45;
                          };
                        inactive_border = "rgba(${builtins.substring 1 6 (theme.neutrals.background_2)}ff)";
                      };
                      layout = "dwindle";
                    };
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
                    decoration = {
                      rounding = 5;
                      blur.enabled = true;
                      shadow = {
                        enabled = false;
                      };
                    };
                    dwindle = {
                      # lua wants bool, not "yes"/"no"
                      preserve_split = true;
                      force_split = 2;
                    };
                    misc = {
                      disable_hyprland_logo = true;
                      disable_splash_rendering = true;
                      vrr = 1;
                      # when opening another program from terminal, swallow it
                      enable_swallow = false;
                      swallow_regex = "^(kitty|lf)$";
                      swallow_exception_regex = "^(wev)$";
                      # suppress start-hyprland warning when not using the watchdog wrapper
                      disable_watchdog_warning = true;
                    };
                    cursor = {
                      inactive_timeout = 5;
                    };
                    debug = {
                      disable_logs = false;
                    };
                    ecosystem = {
                      # don't show update notifications each boot
                      no_update_news = true;
                    };
                    # NB: the animations master switch (`animations.enabled`)
                    # is intentionally NOT set here. Including it in
                    # `hl.config({...})` re-initialises the animation
                    # manager *after* the `hl.curve(...)` / `hl.animation(...)`
                    # calls have already run (config renders after both,
                    # alphabetically), which clobbers each leaf's `bezier`
                    # reference back to the engine default and visibly drops
                    # the snappy `myBezier` curve we define above. Animations
                    # default to enabled in Hyprland, and the per-leaf
                    # `enabled = true` on each `hl.animation(...)` entry
                    # below is sufficient.
                  }
                ];
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
                  # The original hyprlang config used `default` here, which
                  # hyprlang implicitly defined as a snappy ease-out (closer
                  # in feel to `myBezier`). The lua API has no implicit
                  # `default` curve — referencing an undefined name drops
                  # Hyprland into a safe state with no keybinds at startup
                  # — so we use `myBezier` explicitly, which matches the
                  # original perceived snappiness better than `linear`.
                  bezier = "myBezier";
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
                  # Same reasoning as `border` above — original was `default`,
                  # which felt much snappier than `linear`. `linear` here
                  # is what caused the sluggish floating-window fade-in.
                  bezier = "myBezier";
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
              # ---- event handlers --------------------------------------------
              # Each entry renders as one `hl.on(eventName, handler)` call.
              # The list shape is required because we register more than
              # one handler (startup commands + the empty-workspace
              # autoswitch); HM's lua renderer accepts either a single
              # attrset or a list of them and emits one `hl.<name>(...)`
              # call per entry (see
              # nixpkgs/modules/services/window-managers/hyprland.nix
              # `renderCalls`).
              #
              # `lib.optional <bool> <x>` evaluates to `[ <x> ]` or `[]` —
              # cleaner than `lib.mkIf` here because we're building a plain
              # list in let-scope, not contributing to an option, so the
              # module system's `mkIf`-stripping pass never runs.
              on =
                let
                  # Replaces hyprlang `exec-once`. `hl.on("hyprland.start",
                  # ...)` fires once when the compositor starts and runs
                  # each registered command via `hl.exec_cmd`. (The lua
                  # API also exposes a separate `config.reloaded` event
                  # for the per-reload `exec` equivalent — we don't
                  # currently use it, so no second handler is registered
                  # here.)
                  startCmds =
                    lib.optional config.nx.desktop.wallpaper.enable "swaybg -i ${homeDir}/.config/wallpaper-${config.nx.desktop.wallpaper.resolution}.png --mode fill"
                    ++ [
                      "hypridle"
                    ]
                    ++ lib.optional (config.nx.isLaptop == false) "steam -silent -no-cef-sandbox"
                    # default to 70% brightness on laptops
                    ++ lib.optional config.nx.isLaptop "${pkgs.brightnessctl}/bin/brightnessctl s 70%"
                    # default to keyboard backlight off on laptops
                    ++ lib.optional config.nx.isLaptop "${pkgs.brightnessctl}/bin/brightnessctl --device='asus::kbd_backlight' set 0";
                  # NB: the old startup list also exec'd the
                  # `cli.hyprland.switchWorkspaceOnWindowClose` socat
                  # daemon, which has been replaced by the native
                  # `window.close` handler registered below (issue #1961).
                in
                [
                  {
                    _args = [
                      "hyprland.start"
                      (mkLuaInline ''
                        function()
                        ${lib.concatMapStringsSep "\n" (cmd: "  hl.exec_cmd(${lib.generators.toLua { } cmd})") startCmds}
                        end
                      '')
                    ];
                  }
                  # When the last window on a non-1 workspace closes,
                  # switch focus back to the previously-focused workspace.
                  # Lua-native replacement for the previous socat-based
                  # daemon (`cli.hyprland.switchWorkspaceOnWindowClose`)
                  # which polled the hyprland event socket and shelled
                  # out to `hyprctl activeworkspace -j | jq` for each
                  # close event. Same observable behaviour, no daemon,
                  # no socat, no per-event subprocess fork.
                  #
                  # Timing supersedes the first cut of this handler (PR
                  # #1968) which queried `hl.get_active_workspace().is_empty`
                  # synchronously and silently never fired. Tracing the
                  # hyprland 0.55.4 source:
                  #   - `window.close` is emitted at
                  #     src/desktop/view/Window.cpp:2323 inside
                  #     `CWindow::unmapWindow()`.
                  #   - The closing window is detached from its
                  #     workspace's layout space only LATER in the same
                  #     function, at Window.cpp:2392 via
                  #     `g_layoutManager->removeTarget(m_target)` →
                  #     `LayoutManager.cpp:24-26` → `ITarget::assignToSpace(nullptr)`
                  #     at `Target.cpp:25-26`, which calls
                  #     `m_space->remove(...)`.
                  #   - `is_empty` is `getWindows() == 0`
                  #     (`LuaWorkspace.cpp:131-132`), and `getWindows()`
                  #     iterates `m_space->targets()`
                  #     (`Workspace.cpp:438-461`).
                  # So at the synchronous lua callback the closing
                  # window is still in `m_space->targets()`, `is_empty`
                  # is false, and the old guard never matched. The old
                  # socat daemon worked accidentally — it shelled out to
                  # `hyprctl activeworkspace -j` with subprocess
                  # latency, and the IPC `closewindow` was queried
                  # post-removal anyway.
                  #
                  # Fix: receive the closing window via the callback
                  # argument (the lua event dispatch pushes the window
                  # as arg 1, see `LuaEventHandler.cpp:92`), read its
                  # `.workspace` (per `HL.Window` stub line 745 in
                  # `share/hypr/stubs/hl.meta.lua`), and check
                  # `ws.windows == 1` — the closing window IS the one
                  # remaining target, so a count of exactly 1 means
                  # this close empties the workspace. The `ws.active`
                  # gate preserves the original semantic that we only
                  # auto-switch when the closing window's workspace is
                  # the active one (the legacy `hyprctl activeworkspace`
                  # query was implicit-active by construction).
                  #
                  # All four lua symbols used below are present in the
                  # type stubs shipped with hyprland 0.55.4
                  # (`share/hypr/stubs/hl.meta.lua`):
                  #   - `HL.EventName` includes "window.close" (line 19).
                  #   - `HL.Window.workspace : HL.Workspace|nil` (line 745).
                  #   - `HL.Workspace.active : boolean` (line 758).
                  #   - `HL.Workspace.id : integer` (line 765).
                  #   - `HL.Workspace.windows : integer` (line 775).
                  # The workspace-switch dispatcher is
                  # `hl.dsp.focus({ workspace = "previous" })`, the
                  # same form used by every numbered-workspace keybind
                  # below. `"previous"` is monitor-/numbering-
                  # independent (last-focused workspace).
                  #
                  # Nil-guards on `win` and `win.workspace` keep the
                  # callback total in any teardown corner where the
                  # closing window's workspace pointer is already
                  # cleared — the handler no-ops rather than raising a
                  # lua error.
                  {
                    _args = [
                      "window.close"
                      (mkLuaInline ''
                        function(win)
                          if win == nil then return end
                          local ws = win.workspace
                          if ws == nil then return end
                          if not ws.active then return end
                          if ws.id == 1 then return end
                          if ws.windows == 1 then
                            hl.dispatch(hl.dsp.focus({ workspace = "previous" }))
                          end
                        end
                      '')
                    ];
                  }
                ];
              # ---- window rules (lua name: window_rule, underscore) ----------
              # The main module contributes nothing; per-app rules merge in
              # from gaming/, programs/qutebrowser/, etc. via list-merge.
              window_rule = [ ];
              # ---- workspace rules (lua name: workspace_rule, underscore) ----
              # Each entry renders as one `hl.workspace_rule({...})` call,
              # matching the upstream-shipped example at
              # /share/hypr/hyprland.lua. Per-host overrides (e.g. wide outer
              # margins on the ultrawide monitor in machines/navi) merge in
              # via list-merge.
              #
              # Lua-native replacement for the previous shell script
              # `cli.system.setHyprGaps`, which used `hyprctl keyword
              # workspace ...` at session start to install these rules
              # imperatively. That approach was incompatible with the lua
              # parser — `hyprctl keyword` refuses to run against a
              # non-legacy parser ("keyword can't work with non-legacy
              # parsers. Use eval.") — so the rules now live declaratively
              # in the config itself.
              workspace_rule = [ ];
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
                # show quickshell widgets on SUPER_L keydown. We're in lua
                # already, so call `hl.dsp.event(...)` directly rather
                # than shelling out via `hl.dsp.exec_cmd("hyprctl dispatch
                # ...")` — same effect, no fork, no parser round-trip.
                #
                # Modifier state for SUPER_L (the left Super key itself):
                # at keydown the modifier isn't held yet from hyprland's
                # perspective, so the bind matches no-mod + `SUPER_L`.
                # At keyup the modifier IS held (briefly, by release
                # physics), so the release-firing hide bind needs the
                # `SUPER + SUPER_L` form to match. This split mirrors the
                # original hyprlang config (`, SUPER_L, ...` for show /
                # `SUPER, SUPER_L, ...` for hide).
                (mkBind "SUPER_L" ''hl.dsp.event("quickshell:show")'')
                # hide quickshell widgets on SUPER_L keyup. Single
                # top-level entry with `submap_universal = true` replaces the
                # previous three copies (top-level + inside `resize` + inside
                # `exit` submap) — see docs section 8.5.
                (mkBindFlags "SUPER + SUPER_L" ''hl.dsp.event("quickshell:hide")'' {
                  release = true;
                  transparent = true;
                  submap_universal = true;
                })
                # Motions — direction values use the full word form
                # (`"left"`/`"right"`/`"up"`/`"down"`) per the upstream
                # example at /share/hypr/hyprland.lua, not the hyprlang
                # single-letter shorthand (`l`/`r`/`u`/`d`).
                # focus window
                (mkBind "SUPER + h" ''hl.dsp.focus({ direction = "left" })'')
                (mkBind "SUPER + j" ''hl.dsp.focus({ direction = "down" })'')
                (mkBind "SUPER + k" ''hl.dsp.focus({ direction = "up" })'')
                (mkBind "SUPER + l" ''hl.dsp.focus({ direction = "right" })'')
                # move window
                (mkBind "SUPER + SHIFT + H" ''hl.dsp.window.move({ direction = "left" })'')
                (mkBind "SUPER + SHIFT + J" ''hl.dsp.window.move({ direction = "down" })'')
                (mkBind "SUPER + SHIFT + K" ''hl.dsp.window.move({ direction = "up" })'')
                (mkBind "SUPER + SHIFT + L" ''hl.dsp.window.move({ direction = "right" })'')
                # switch workspace. `hl.dsp.workspace` is a namespace TABLE
                # (holding `toggle_special`, `move`, `swap_monitors`,
                # `rename`) — not a callable dispatcher. The actual
                # workspace-switch dispatcher is `hl.dsp.focus({ workspace =
                # N })`, per the upstream example at
                # /share/hypr/hyprland.lua lines 277-280.
                (mkBind "SUPER + 1" "hl.dsp.focus({ workspace = 1 })")
                (mkBind "SUPER + 2" "hl.dsp.focus({ workspace = 2 })")
                (mkBind "SUPER + 3" "hl.dsp.focus({ workspace = 3 })")
                (mkBind "SUPER + 4" "hl.dsp.focus({ workspace = 4 })")
                (mkBind "SUPER + 5" "hl.dsp.focus({ workspace = 5 })")
                (mkBind "SUPER + 6" "hl.dsp.focus({ workspace = 6 })")
                (mkBind "SUPER + 7" "hl.dsp.focus({ workspace = 7 })")
                (mkBind "SUPER + 8" "hl.dsp.focus({ workspace = 8 })")
                (mkBind "SUPER + 9" "hl.dsp.focus({ workspace = 9 })")
                (mkBind "SUPER + TAB" ''hl.dsp.focus({ workspace = "previous" })'')
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
                # scroll through existing workspaces (same dispatcher-rename
                # as the numbered switches above).
                (mkBind "SUPER + mouse_down" ''hl.dsp.focus({ workspace = "e+1" })'')
                (mkBind "SUPER + mouse_up" ''hl.dsp.focus({ workspace = "e-1" })'')
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
                # Inline lua closure (formerly the
                # `system.inputs.toggleTouchpad` shell script in scripts.nix).
                # The lua API is write-only for per-device `enabled`
                # (`hl.device` is declared `fun(spec): nil`, and no
                # `get_device` exists on `HL.API`), so the toggle has to
                # track its own intended state. Holding that state in a
                # closure inside the same lua VM as the device config means
                # it resets in lockstep with `hyprctl reload` — fixing the
                # desync the old shell script had where its status file in
                # `$XDG_RUNTIME_DIR` outlived the device's config-default
                # reset. Same IIFE-with-private-state pattern used by the
                # submap auto-reset binds above.
                (lib.mkIf config.nx.isLaptop (
                  mkBind "SUPER + p" ''
                    (function()
                      local enabled = true
                      local name = "asup1415:00-093a:300c-touchpad"
                      return function()
                        enabled = not enabled
                        hl.device({ name = name, enabled = enabled })
                        hl.dsp.event("quickshell:osd:touchpad:" .. (enabled and "on" or "off"))
                      end
                    end)()
                  ''
                ))
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
