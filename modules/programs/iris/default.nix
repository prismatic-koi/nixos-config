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

  # NOTE on the daemon's socket path and IRIS_DAEMON_SOCK
  # ---------------------------------------------------------------------------
  # An earlier revision of this module set `IRIS_DAEMON_SOCK` in the daemon's
  # service environment, mirroring the wording of issue #1663. That was wrong.
  #
  # In the iris codebase, `IRIS_DAEMON_SOCK` is **not** the daemon's bind
  # address: the daemon resolves its bind path purely from XDG_STATE_HOME via
  # `internal/iris/paths.go::ResolvePaths()`, and all in-tree clients
  # (`cmd/iris/{main,tui,stats}.go`) use the same `ResolvePaths().Sock` —
  # never `os.Getenv("IRIS_DAEMON_SOCK")`. The env var is reserved for a
  # different role: the daemon sets it on each pi child it spawns, pointing
  # at that child's per-session **harness** socket. The prism extension reads
  # it as a trigger to register iris tool overrides and to dial the harness
  # socket. See `internal/iris/supervisor.go` (writer) and
  # `modules/programs/prism/pi/extensions/prism.ts` (reader), and
  # `docs/daemon-mode-design.md` §3.4 for the explicit contract.
  #
  # If we exported `IRIS_DAEMON_SOCK=<daemon client socket>` in the daemon's
  # service environment, and that value ever escaped into a user login shell
  # (e.g. via `systemctl --user import-environment` on Linux, or any pi
  # process inheriting it from launchd on Darwin), the prism extension would
  # dial the daemon's client socket as if it were a harness socket — wrong
  # protocol, wrong endpoint. The conservative fix is to leave the env var
  # unset on the daemon side and rely on the binary's XDG-derived default.
  # Clients do the same; the contract is path-based, not env-var-based.
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
            # Intentionally no Environment= block. See the comment at the top
            # of this file for why `IRIS_DAEMON_SOCK` is *not* set here.
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
            # Intentionally no EnvironmentVariables block. See the comment at
            # the top of this file for why `IRIS_DAEMON_SOCK` is *not* set here.
            # launchd has no journal — capture stdout/stderr to per-user logs.
            StandardOutPath = "${homeDir}/Library/Logs/iris/stdout.log";
            StandardErrorPath = "${homeDir}/Library/Logs/iris/stderr.log";
          };
        };
      })
    ];
  };
}
