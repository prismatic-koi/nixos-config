{
  config,
  lib,
  pkgs,
  ...
}:
let
  prismPkg = pkgs.callPackage ../../../pkgs/prism.nix { };
  prismd = "${prismPkg}/bin/prismd";

  cfg = config.nx.programs.prism.muxDaemon;
in
{
  # The mux daemon (prismd mux) is the long-running per-user process that
  # owns the prism-native multiplexer's session tree, the Unix-socket API,
  # and the periodic snapshot loop. This module registers a user-level
  # service supervisor so the daemon starts at login and restarts on crash.
  #
  # Important: this module only ensures the daemon EXISTS. Whether the
  # rest of prism (spawn / cleanup / switch / nav) actually routes through
  # the mux daemon vs. the legacy tmux path is governed by the
  # PRISM_USE_MUX env var, which is plumbed by the cutover PR (#2158).
  # The daemon's mere presence is harmless when PRISM_USE_MUX is unset
  # \u2014 nothing will dial its socket.
  options = {
    nx.programs.prism.muxDaemon = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = config.nx.programs.prism.enable;
        defaultText = lib.literalExpression "config.nx.programs.prism.enable";
        description = ''
          Whether to register the prismd-mux user service (systemd user
          unit on Linux, launchd user agent on Darwin). When true, the
          mux daemon starts at login and restarts on crash. The flag is
          orthogonal to PRISM_USE_MUX (#2158) \u2014 the daemon's presence
          is harmless when the rest of prism is still using tmux.
        '';
      };

      restartSec = lib.mkOption {
        type = lib.types.int;
        default = 5;
        description = ''
          systemd RestartSec value, in seconds. Applied to the Linux
          user unit only; launchd's ThrottleInterval is a separate knob.
          Default of 5 s is short enough that a transient crash recovers
          before the user notices, long enough to avoid a tight
          restart loop on an unrecoverable startup failure.
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      # ----------------------------------------------------------------
      # Linux \u2014 systemd user unit.
      #
      # Mirrors the shape of modules/programs/prism/tmux.nix's
      # prism-restore unit (Type=oneshot at login) but is long-lived
      # (Type=simple, Restart=on-failure) because the daemon owns the
      # whole multiplexer lifecycle, not a one-off restore action.
      # ----------------------------------------------------------------
      (lib.mkIf pkgs.stdenv.isLinux {
        home-manager.users.${config.nx.username} = {
          systemd.user.services.prismd-mux = {
            Unit = {
              Description = "Prism multiplexer daemon (prismd mux)";
              # default.target is the login-time analogue for user
              # services \u2014 the unit starts when the user session
              # comes up, not when graphical-session.target fires
              # (the mux daemon is headless and does not depend on a
              # graphical session).
              After = [ "default.target" ];
              # Soft dep on graphical-session.target so the snapshot
              # restore happens before the user opens a terminal,
              # without blocking session start if graphical-session
              # is delayed or absent (e.g. SSH login).
              Wants = [ "default.target" ];
            };
            Service = {
              Type = "simple";
              # --foreground means lifecycle.Run executes in this very
              # process, with systemd as the supervisor. The daemon
              # installs its own SIGTERM/SIGINT handlers, so KillMode
              # left at the default (control-group) is correct \u2014
              # systemd sends SIGTERM, lifecycle.Run does its graceful
              # shutdown (final snapshot, drain, unlink PID file),
              # systemd reaps.
              ExecStart = "${prismd} mux start --foreground";
              # Restart on any abnormal exit. Exit code 2 is
              # ErrAlreadyRunning (another process holds the PID file)
              # \u2014 do NOT restart on that one, restarting would
              # repeatedly fail the same check until the existing
              # daemon is dealt with. on-failure is the right setting:
              # it covers SIGKILL, OOM, and panic without flapping on
              # "already running".
              Restart = "on-failure";
              RestartSec = toString cfg.restartSec;
              # ExitCode 2 (ErrAlreadyRunning) is treated as a clean
              # exit \u2014 do not count against the restart budget and
              # do not retry.
              RestartPreventExitStatus = "2";
              # Quiet, structured output to journald.
              StandardOutput = "journal";
              StandardError = "journal";
              SyslogIdentifier = "prismd-mux";
              # Generous timeout for the SIGTERM \u2192 SIGKILL escalation
              # so a slow snapshot save on a heavily-loaded host has
              # time to finish. Matches lifecycle.DefaultStopGrace.
              TimeoutStopSec = "15";
            };
            Install = {
              # WantedBy default.target so `systemctl --user enable`
              # at first login (handled by home-manager's
              # systemd.user.startServices) makes the service start
              # on every subsequent login.
              WantedBy = [ "default.target" ];
            };
          };
        };
      })

      # ----------------------------------------------------------------
      # Darwin \u2014 launchd user agent.
      #
      # KeepAlive { SuccessfulExit = false } is the launchd equivalent
      # of systemd's Restart=on-failure: relaunch only when the agent
      # exits abnormally. RunAtLoad starts the agent at login.
      # ----------------------------------------------------------------
      (lib.mkIf pkgs.stdenv.isDarwin {
        home-manager.users.${config.nx.username}.launchd.agents.prismd-mux = {
          enable = true;
          config = {
            ProgramArguments = [
              prismd
              "mux"
              "start"
              "--foreground"
            ];
            RunAtLoad = true;
            KeepAlive = {
              # Relaunch only when the agent exits non-zero. A clean
              # `prismd mux stop` (exit 0) leaves the agent stopped
              # until the user explicitly restarts it.
              SuccessfulExit = false;
            };
            ThrottleInterval = cfg.restartSec;
            # Logs land under /tmp so they are visible from a default
            # `tail -f` without further configuration. Production
            # debugging should still prefer `log show --predicate
            # 'process == \"prismd\"'` but the file path is here for
            # convenience during first rollout.
            StandardOutPath = "/tmp/prismd-mux.log";
            StandardErrorPath = "/tmp/prismd-mux.log";
          };
        };
      })
    ]
  );
}
