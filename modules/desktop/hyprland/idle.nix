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
  config = lib.mkIf (config.nx.desktop.hyprland.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben = {
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
              lock_cmd = "hyprctl dispatch exec hyprlock";
            };
            listener = [
              (lib.mkIf lockCfg.enable {
                timeout = lockCfg.duration;
                on-timeout = "hyprctl dispatch exec hyprlock";
              })
              (lib.mkIf screenCfg.enable {
                timeout = screenCfg.duration;
                on-timeout = "hyprctl dispatch dpms off";
                on-resume = "hyprctl dispatch dpms on";
              })
              (lib.mkIf suspendCfg.enable {
                timeout = suspendCfg.duration;
                on-timeout = "systemctl suspend";
                on-resume = "hyprctl dispatch exec hyprlock";
              })
            ];
          };
        };
    };
  };
}
