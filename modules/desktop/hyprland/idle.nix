{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.desktop.hyprland = {
      lockTimeout.enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "whether to lock the screen due to idle";
      };
      lockTimeout.duration = lib.mkOption {
        type = lib.types.int;
        # default 30 minutes
        default = 1800;
        description = "time before locking the screen";
      };
      screenTimeout.enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "whether to turn off the screen due to idle";
      };
      screenTimeout.duration = lib.mkOption {
        type = lib.types.int;
        # default 1 hour
        default = 3600;
        description = "time before turning off the screen";
      };
      suspendTimeout.enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "whether to suspend the system due to idle";
      };
      suspendTimeout.duration = lib.mkOption {
        type = lib.types.int;
        # default 2 hours
        default = 7200;
        description = "time before suspending the system";
      };
    };
  };
  config = lib.mkIf (config.nx.desktop.hyprland.enable && pkgs.stdenv.hostPlatform.isLinux) {
    home-manager.users.${config.nx.username} = {
      services.hypridle =
        let
          lockCfg = config.nx.desktop.hyprland.lockTimeout;
          screenCfg = config.nx.desktop.hyprland.screenTimeout;
          suspendCfg = config.nx.desktop.hyprland.suspendTimeout;
        in
        {
          enable = true;
          settings = {
            general = {
              # `hyprctl dispatch` under the lua parser evaluates args as
              # lua source, so legacy `exec hyprlock` fails to parse.
              # Pass the lua-call form instead, matching the pattern in
              # the upstream-shipped example at /share/hypr/hyprland.lua
              # (line ~260): `hyprctl dispatch 'hl.dsp.exit()'`.
              lock_cmd = ''hyprctl dispatch 'hl.dsp.exec_cmd("hyprlock")' '';
            };
            listener = [
              (lib.mkIf lockCfg.enable {
                timeout = lockCfg.duration;
                on-timeout = ''hyprctl dispatch 'hl.dsp.exec_cmd("hyprlock")' '';
              })
              (lib.mkIf screenCfg.enable {
                timeout = screenCfg.duration;
                # Under the lua parser `hl.dsp.dpms` takes a table with
                # an `action` field — see
                # `src/config/lua/bindings/LuaBindingsDispatchers.cpp`
                # (`hlDpms` → `Internal::tableToggleAction`) and
                # `parseToggleStr` in `LuaBindingsInternal.cpp`, which
                # accepts "on"/"off"/"enable"/"disable"/"toggle".
                on-timeout = ''hyprctl dispatch 'hl.dsp.dpms({ action = "off" })' '';
                on-resume = ''hyprctl dispatch 'hl.dsp.dpms({ action = "on" })' '';
              })
              (lib.mkIf suspendCfg.enable {
                timeout = suspendCfg.duration;
                on-timeout = "systemctl suspend";
                on-resume = ''hyprctl dispatch 'hl.dsp.exec_cmd("hyprlock")' '';
              })
            ];
          };
        };
    };
  };
}
