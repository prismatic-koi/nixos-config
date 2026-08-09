# prism exporter — Prometheus metrics for prism's own operation.
#
# Part of the prism telemetry train (issue #2699). This module is the
# first child (#2700): it runs the daemon. The Alloy scrape that
# reads it is #2701 and deliberately lands in a separate PR, so this
# module changes no Alloy config.
#
# ── Why a user unit, not a system unit ──────────────────────────────
#
# The exporter reads `$XDG_STATE_HOME/prism/prism.db`, which lives in
# the user's home, and writes its tail-cursor state file next to it.
# It also reads the sibling `$XDG_STATE_HOME/prism/usage/` snapshot
# directory to map an account name to its org ID for the account
# dimension of the cost counters (#2704); that path resolves the same
# way `prism.db` does, so no extra flag is needed.
# A system unit would have to reach across the user boundary to do
# that; a DynamicUser system unit could not do it at all. So this is
# a `systemd.user.service`, in the same shape as `prism-restore` in
# `modules/programs/prism/tmux.nix`.
#
# Consequence for #2701: the scrape target is a loopback port, so
# Alloy — which is a system-scope service — reaches it without
# needing any user-scope access. The unit name is
# `prism-exporter.service` in the USER manager. It is therefore
# invisible to Alloy's embedded systemd collector, which dials the
# system bus only (see the module header of
# `modules/services/alloy/default.nix`). If you want unit health for
# this daemon, add it to `bespokeUserUnits` in that module.
#
# ── Why loopback only ───────────────────────────────────────────────
#
# The endpoint has no authentication of its own. Alloy runs on the
# same host, so binding loopback removes the whole question. Before
# you move it to 0.0.0.0, put an auth boundary in front of it.
#
# ── Restart behaviour ───────────────────────────────────────────────
#
# `Restart = "always"` with rate limiting turned off. The daemon
# exits non-zero when `prism.db` is not there yet, which is the
# normal state of a freshly installed machine until prism first runs.
# Without `StartLimitIntervalSec = 0` systemd would give up after a
# few attempts and leave the unit failed until the next login.
#
# ── Lifetime: no lingering ──────────────────────────────────────────
#
# `systemd --user` starts at first login and stops at last logout, so
# the exporter runs while a user session exists and not otherwise. On
# a booted but logged-out navi or tui there is no /metrics endpoint,
# and #2701's scrape target reports `up=0`.
#
# That is deliberate. `users.users.<name>.linger = true` would fix it
# in one line, and this module does NOT set it, because lingering is
# a property of the user manager rather than of one unit. Evaluating
# both host configs shows EIGHT pre-existing user units that are
# `WantedBy` a boot-reachable target and would therefore start at
# boot:
#
#   default.target  bitwarden-prefetch, clear-qutebrowser-history,
#                   mpd, mpdris2, qutebrowser-setup
#   timers.target   bitwarden-prefetch,
#                   qutebrowser-clear-history-timer,
#                   systemd-user-unit-metrics
#
# The material one is `mpd`. It binds `network.listenAddress =
# "0.0.0.0"` on port 6600 (`modules/services/mpd.nix`), and
# `machines/navi/configuration.nix` opens TCP 6600 in the firewall.
# Today mpd reaches the network only while somebody is logged in.
# With lingering it would do so from boot. A telemetry PR is the
# wrong place to decide that.
#
# So the `up=0`-when-logged-out question belongs to #2701, which owns
# the scrape target and its operator-verified AC. Whoever takes it
# has two options: enable lingering, having decided the mpd exposure
# is acceptable, or accept `up=0` and account for it in the alerting.
#
# ── Hardening ───────────────────────────────────────────────────────
#
# The directives below are the seccomp and prctl based ones, which
# apply to a user unit without any privilege.
#
# Two families are deliberately absent:
#
#   * The mount-namespace ones (`ProtectSystem`, `ProtectHome`,
#     `PrivateTmp`). Their behaviour in the user manager depends on
#     unprivileged user namespaces being available, and a wrong guess
#     there fails the unit at switch time rather than at review time.
#
#   * `CapabilityBoundingSet`. `systemd.exec(5)` states the
#     capability directives are "only available for system services,
#     or for services running in per-user instances of the service
#     manager in which case `PrivateUsers=` is implicitly enabled".
#     This unit sets no `PrivateUsers`, so the directive is not
#     available to it. `PR_CAPBSET_DROP` needs `CAP_SETPCAP` and
#     returns `EPERM` for an unprivileged process, and systemd
#     tolerates that `EPERM` only when the bounding set is already
#     empty. The user manager inherits PID 1's full bounding set, so
#     the exec aborts with `EXIT_CAPABILITIES` (229) — and with
#     `Restart = "always"` and no start-rate limit, it would do so
#     every 10 seconds, forever. Do not add it back.
#
# The daemon holds a read-only SQLite handle and one loopback
# listener, so the residual surface is small.
#
# ── Darwin (#2705) ───────────────────────────────────────────────────
#
# m4mac's Alloy daemon (`modules/services/alloy/default.nix`) runs as
# root, because nix-darwin's `launchd.daemons` has no `DynamicUser`
# equivalent and Alloy's job (reading arbitrary node_exporter
# collectors) genuinely needs host-wide reach. The exporter's job is
# narrower: read one user's `prism.db`, nothing else. Running it as
# root to do that would be strictly more privilege than the task
# needs, for no benefit — root does not make SQLite go faster.
#
# So this is a `launchd.daemons` entry (system-scope, so it starts at
# boot without a login, matching the systemd-user unit's `RunAtLoad`+
# restart-forever behaviour on Linux) with `UserName` set to the
# prism user. launchd drops privilege to that user before exec, the
# same mechanism macOS system daemons use throughout (e.g.
# `/System/Library/LaunchDaemons` entries with a `UserName` key). The
# daemon then reads `prism.db` under that user's home with exactly
# the permission a normal login session would have — no more.
#
# This is deliberately NOT a `launchd.agents` (user-scope, LaunchAgent)
# entry, unlike `flake-update-notifier`'s Darwin half. An agent only
# runs while that user is logged in — graphical-session-gated,
# effectively — which would reproduce the exact `up=0`-while-logged-out
# gap the Linux systemd-user unit already has (see "Lifetime: no
# lingering" above) but for a worse reason: on Linux that gap is an
# accepted tradeoff against opening `linger` for unrelated units
# (mpd). On Darwin there is no such tradeoff to make — a `UserName`-
# scoped system daemon gets both boot-time start AND least privilege
# at once, so there is no reason to accept the gap here.
{
  config,
  lib,
  pkgs,
  isLinux,
  ...
}:
let
  cfg = config.nx.services.prismExporter;
  username = config.nx.username;
  prismPkg = pkgs.callPackage ../../../pkgs/prism.nix { };
  isDarwin = !isLinux;

  # machines/m4mac/configuration.nix sets `users.users.${username}.home
  # = "/Users/${username}"`. A `launchd.daemons` entry runs with no
  # login shell and no environment inherited from the target user, so
  # `$HOME` and `$XDG_STATE_HOME` are both unset at exec time — unlike
  # the Linux `systemd.user.service`, which runs inside that user's own
  # session and gets `$XDG_STATE_HOME` resolved for it. The exporter's
  # `--db` default (`cmd/db.go`'s `dbPath()`) falls back to
  # `$HOME/.local/state` when `$XDG_STATE_HOME` is unset, but `$HOME`
  # itself is unset too under launchd, so that fallback would resolve
  # against whatever directory launchd happens to exec from — not the
  # user's home. So the Darwin daemon passes `--db` and `--state`
  # explicitly, spelling out the same `~/.local/state/prism/...` layout
  # the Linux unit gets for free from its session.
  darwinHome = "/Users/${username}";
  darwinDBPath = "${darwinHome}/.local/state/prism/prism.db";
  darwinStatePath = "${darwinHome}/.local/state/prism/exporter-state.json";
in
{
  options.nx.services.prismExporter = {
    enable = lib.mkEnableOption "prism Prometheus exporter (serves /metrics for Alloy to scrape)";

    listenAddress = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1";
      description = ''
        Address the exporter binds.

        Defaults to loopback. The `/metrics` endpoint has no
        authentication of its own, and Alloy scrapes it from the same
        host, so there is no reason to expose it further. Before you
        change this, put an authenticating proxy in front of it.
      '';
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 19891;
      description = ''
        TCP port the exporter serves `/metrics` on.

        Must match `DefaultPort` in
        `modules/programs/prism/prism/internal/exporter/exporter.go`,
        and the scrape target #2701 adds to
        `modules/services/alloy/default.nix`.

        The value sits outside the 9100-9999 band that the Prometheus
        project allocates to community exporters, so it cannot
        collide with a future exporter default on this host.
      '';
    };
  };

  # Guarded by the `isLinux` specialArg rather than `lib.mkIf
  # pkgs.stdenv.isLinux`, for the same reason the alloy module does
  # it: `optionalAttrs` collapses at Nix time, before the module
  # system walks the definition tree.
  config = lib.mkIf (cfg.enable && config.nx.programs.prism.enable) (
    lib.mkMerge [
      (lib.optionalAttrs isLinux {
        home-manager.users.${username} = {
          systemd.user.services.prism-exporter = {
            Unit = {
              Description = "Prometheus exporter for prism operational metrics";
              Documentation = [ "https://github.com/prismatic-koi/nixos-config/issues/2700" ];
              # Retry for as long as it takes. See "Restart behaviour"
              # in the module header.
              StartLimitIntervalSec = 0;
            };

            Service = {
              Type = "simple";
              # systemd parses ExecStart itself — there is no shell in the
              # loop. Its quoting rules resemble the POSIX ones but are not
              # identical: a closing quote must be followed by whitespace,
              # so the shell's `'it'\''s'` form for an embedded single
              # quote is mis-parsed. escapeShellArg is therefore correct
              # here for any address without a single quote in it, which
              # covers every IP and hostname. It earns its place only if
              # listenAddress ever carries whitespace.
              ExecStart = lib.concatStringsSep " " [
                "${prismPkg}/bin/prism exporter"
                "--listen ${lib.escapeShellArg cfg.listenAddress}"
                "--port ${toString cfg.port}"
              ];
              Restart = "always";
              RestartSec = 10;

              # Logs go to the journal. The daemon writes one line per
              # operational event (start, corrupt state file, cursor
              # clamp, failed scrape), not one per scrape.
              StandardOutput = "journal";
              StandardError = "journal";

              # Every directive here is seccomp or prctl based, so it applies
              # to an unprivileged user unit. See "Hardening" in the module
              # header for the two families that are deliberately absent, and
              # why CapabilityBoundingSet in particular must not come back.
              NoNewPrivileges = true;
              LockPersonality = true;
              MemoryDenyWriteExecute = true;
              RestrictRealtime = true;
              RestrictSUIDSGID = true;
              RestrictNamespaces = true;
              RestrictAddressFamilies = "AF_INET AF_INET6 AF_UNIX";
              SystemCallArchitectures = "native";
              SystemCallFilter = "@system-service";
            };

            Install = {
              # default.target, not graphical-session.target: the
              # exporter is a headless daemon and must run on a machine
              # nobody has logged into graphically.
              WantedBy = [ "default.target" ];
            };
          };
        };
      })

      (lib.optionalAttrs isDarwin {
        launchd.daemons.prism-exporter = {
          serviceConfig = {
            # Drop privilege to the prism user before exec -- see the
            # module header "Darwin (#2705)" for why this is a
            # UserName-scoped system daemon rather than root or a
            # per-user LaunchAgent.
            UserName = username;

            # /bin/wait4path guards against the daemon starting before
            # the /nix APFS volume is mounted at boot (the prism
            # binary lives in the store) -- same guard as
            # launchd.daemons.alloy in modules/services/alloy/default.nix.
            #
            # --db and --state are explicit because, unlike the Linux
            # systemd.user.service, a launchd system daemon execs with
            # no $HOME -- see the module header for why the defaults
            # cannot be relied on here.
            ProgramArguments = [
              "/bin/sh"
              "-c"
              (lib.concatStringsSep " " [
                "/bin/wait4path ${prismPkg}/bin/prism &&"
                "exec ${prismPkg}/bin/prism exporter"
                "--listen ${lib.escapeShellArg cfg.listenAddress}"
                "--port ${toString cfg.port}"
                "--db ${lib.escapeShellArg darwinDBPath}"
                "--state ${lib.escapeShellArg darwinStatePath}"
              ])
            ];

            # RunAtLoad + KeepAlive is the launchd equivalent of the
            # Linux unit's WantedBy default.target (starts at boot
            # without a manual launchctl load) plus Restart = always
            # (self-heals on crash, and on the "prism.db not there yet"
            # exit path a freshly installed machine hits before prism
            # first runs -- see "Restart behaviour" above).
            RunAtLoad = true;
            KeepAlive = true;

            StandardOutPath = "/var/log/prism-exporter.log";
            StandardErrorPath = "/var/log/prism-exporter.log";
          };
        };
      })
    ]
  );
}
