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
  # system walks the definition tree. Darwin is #2705.
  config = lib.mkIf (cfg.enable && config.nx.programs.prism.enable) (
    lib.optionalAttrs isLinux {
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
    }
  );
}
