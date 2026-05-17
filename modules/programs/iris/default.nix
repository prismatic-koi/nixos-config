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

  # Resolve the absolute store path to the prism PI extension file
  # (prism.ts). The supervisor passes this path verbatim to
  # `pi --extension <path>`; if it is empty, `--extension` is silently
  # omitted and every pi child runs as vanilla pi with no iris awareness
  # (see issue #1752 for the diagnosis).
  #
  # Source-sharing: the canonical `prism.ts` lives at
  # `../prism/pi/extensions/prism.ts`. Both this module and
  # `modules/programs/prism/pi.nix` reference that same file — there is no
  # duplicate copy of the extension source. We rebuild the trivial
  # single-file derivation here (rather than reading
  # `config.nx.programs.prism.piExtensionDir`) so that iris does not
  # silently break when prism is disabled on a host.
  prismExtensionDir = pkgs.runCommand "prism-pi-extension" { } ''
    mkdir -p $out
    cp ${../prism/pi/extensions/prism.ts} $out/prism.ts
  '';
  prismExtensionPath = "${prismExtensionDir}/prism.ts";

  irisConfig = {
    pi_extension_path = prismExtensionPath;
    pi_provider = cfg.pi.provider;
    pi_model = cfg.pi.model;
    pi_thinking = cfg.pi.thinking;
  };

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

      # pi.{provider,model,thinking} — LLM routing for pi children spawned
      # by the iris supervisor. See issue #1777: without these, pi 0.72.x
      # falls back to a built-in default that picks
      # `github-copilot/gpt-5.4`, for which most users have no auth, and
      # every turn fails with `400 bad request: Authorization header is
      # badly formatted`.
      #
      # The iris supervisor passes `--provider`/`--model`/`--thinking` to
      # pi on the command line (see
      # `internal/iris/supervisor.go::buildPIArgs`). The defaults below
      # match the values the (now-deprecated) per-session settings.json
      # injection used to write, plus `medium` thinking as a sensible
      # default for the coder role.
      #
      # Deliberately self-contained: this module does NOT read from
      # `config.nx.programs.prism.*`. Iris must continue to function with
      # prism disabled — see the surrounding comment about
      # `prismExtensionDir` for the same rationale applied to the
      # extension path.
      pi = {
        provider = lib.mkOption {
          type = lib.types.str;
          default = "anthropic";
          example = "openai";
          description = ''
            LLM provider passed to pi via `--provider <value>`. Written
            to `~/.config/iris/config.json` as `pi_provider`. Set to the
            empty string to omit the flag and let pi pick its own
            default (not recommended — see issue #1777).
          '';
        };
        model = lib.mkOption {
          type = lib.types.str;
          default = "claude-sonnet-4-20250514";
          example = "gpt-5";
          description = ''
            LLM model passed to pi via `--model <value>`. Written to
            `~/.config/iris/config.json` as `pi_model`. Must be a model
            that the configured provider exposes.
          '';
        };
        thinking = lib.mkOption {
          type = lib.types.str;
          default = "medium";
          example = "high";
          description = ''
            Thinking level passed to pi via `--thinking <value>`. Written
            to `~/.config/iris/config.json` as `pi_thinking`. Typical
            values are `off`, `low`, `medium`, `high` — pi validates the
            value, iris does not.
          '';
        };
      };
    };
  };

  config = lib.mkIf cfg.enable {
    home-manager.users.${username} = lib.mkMerge [
      {
        home.packages = [ pkgs.iris ];

        # Write ~/.config/iris/config.json so the iris daemon knows where
        # the prism PI extension lives. Without this, `PIExtensionPath`
        # defaults to "" and the supervisor omits `--extension` from every
        # pi child — see issue #1752.
        xdg.configFile."iris/config.json".text = builtins.toJSON irisConfig;
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
            # Always restart — iris is an always-up user daemon, so even a
            # clean SIGTERM exit (e.g. `systemctl --user kill iris`) should
            # bring it back. `systemctl --user stop iris` remains the explicit
            # "keep it down" verb and is unaffected.
            Restart = "always";
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
            # KeepAlive = true: launchd analogue of Restart=always.
            # Always restart the daemon, regardless of exit code. Clean exit 0
            # from SIGTERM, internal os.Exit(0), or any other graceful path
            # still triggers a restart — the daemon should be up whenever the
            # user is logged in. The "keep it down" verbs are
            # `launchctl unload` / `launchctl bootout` / `launchctl stop`,
            # not a clean exit.
            KeepAlive = true;
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
