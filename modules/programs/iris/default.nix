{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.nx.programs.iris;
  username = config.nx.username;
  homeDir = config.home-manager.users.${username}.home.homeDirectory;

  # Canonical per-user socket path. Aligned with the default that the iris Go
  # binary resolves in `internal/iris/paths.go` (`$XDG_STATE_HOME/iris/iris.sock`,
  # which on a default home defaults to `~/.local/state/iris/iris.sock`). The
  # env var is therefore redundant rather than required — see issue #1663.
  #
  # macOS does not export `XDG_RUNTIME_DIR`, so we use the same XDG_STATE_HOME
  # path on both platforms to keep the contract uniform. Clients dial
  # `IRIS_DAEMON_SOCK` and get the same answer regardless of OS.
  irisDaemonSock = "${homeDir}/.local/state/iris/iris.sock";
in
{
  options = {
    nx.programs.iris = {
      enable = lib.mkEnableOption "iris — daemon-mode successor to prism (codename, D-2+)" // {
        default = false;
      };
    };
  };

  config = lib.mkIf cfg.enable {
    home-manager.users.${username} = lib.mkMerge [
      {
        home.packages = [ pkgs.iris ];
      }

      # ── Linux: systemd user service ─────────────────────────────────────────
      (lib.mkIf pkgs.stdenv.isLinux {
        systemd.user.services.iris = {
          Unit = {
            Description = "iris daemon — daemon-supervised pi RPC + per-tool sandboxing";
          };

          Service = {
            Type = "simple";
            ExecStart = "${pkgs.iris}/bin/iris daemon";
            Restart = "on-failure";
            RestartSec = 5;
            # Export the canonical socket path so anything the daemon spawns
            # (and any login shell that inherits this env via `systemctl --user
            # show-environment`) agrees with the daemon's bind path.
            Environment = [
              "IRIS_DAEMON_SOCK=${irisDaemonSock}"
            ];
          };

          Install = {
            WantedBy = [ "default.target" ];
          };
        };
      })

      # ── Darwin: launchd user agent ──────────────────────────────────────────
      (lib.mkIf pkgs.stdenv.isDarwin {
        launchd.agents.iris = {
          enable = true;
          config = {
            Label = "local.iris.daemon";
            ProgramArguments = [
              "${pkgs.iris}/bin/iris"
              "daemon"
            ];
            RunAtLoad = true;
            # Restart on crash but not on clean exit — `iris daemon` exiting 0
            # is an explicit shutdown request and should not loop.
            KeepAlive = {
              SuccessfulExit = false;
            };
            EnvironmentVariables = {
              IRIS_DAEMON_SOCK = irisDaemonSock;
            };
            # launchd has no journal — capture stdout/stderr to per-user logs.
            StandardOutPath = "${homeDir}/Library/Logs/iris/stdout.log";
            StandardErrorPath = "${homeDir}/Library/Logs/iris/stderr.log";
          };
        };
      })
    ];
  };
}
