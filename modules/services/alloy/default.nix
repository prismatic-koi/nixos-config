# Grafana Alloy — cross-platform telemetry collector.
#
# Part of the fleet telemetry train (issue #2458). The first child
# (issue #2460) added host metrics. Issue #2461 added a Syncthing
# `/metrics` scrape on the Linux hosts and issue #2697 extended it to
# m4mac; issue #2787 then dropped the Bearer token that scrape used
# — see the "Syncthing scrape" comment below. Loki log shipping and
# other signals are still out of scope here — a
# commented seam is left in the generated Alloy config so a later
# train can wire `loki.write` without redesigning the module surface.
#
# Every scrape forwards to the single `prometheus.remote_write`
# below. Do not add a second one.
#
# ── Cross-platform shape ─────────────────────────────────────────────
#
# Modelled on `modules/programs/tailscale-client/default.nix`: an
# `nx.*` enable option with two platform branches:
#
#   * NixOS (navi, tui) — reuses the upstream nixpkgs
#     `services.alloy` module, which provides a systemd unit with
#     DynamicUser, StateDirectory, and reload-on-config-change.
#     Config is written to `/etc/alloy/config.alloy` via
#     `environment.etc` so the nixpkgs module can pick it up and
#     trigger a reload on switch.
#
#   * Darwin (m4mac) — nix-darwin has no first-class services.alloy,
#     so we roll a `launchd.daemons.alloy` unit (system-scope, root)
#     ourselves. RunAtLoad + KeepAlive so it survives crashes and
#     restarts on boot without depending on a user login. Config
#     lives at the same `/etc/alloy/config.alloy` path on both
#     platforms for consistency.
#
# ── Auth boundary ────────────────────────────────────────────────────
#
# v1 requires NO application auth token. The tailnet IS the auth
# boundary: the home-ops headscale ACL policy grants only our user
# (`ben@`) reach into the ts-metrics-ingest node on tcp/9090.
# Nothing else on the tailnet can talk to it. If a token is ever
# required in the future it lives in sops — a commented seam is left
# in the generated River config below.
#
# ── Metric collection: prometheus.exporter.unix cross-platform ──────
#
# The AC wording for #2460 said "hostmetrics" (as in the OpenTelemetry
# `otelcol.receiver.hostmetrics` component). That component is NOT
# registered in nixpkgs' `grafana-alloy` 1.17.1 build — the build
# targets the `collector/` subpackage of the alloy repo, and
# `internal/component/otelcol/receiver/` in that tree does not
# contain a `hostmetrics/` subdirectory. Adding it would require
# either upgrading nixpkgs' alloy version (out of scope for this PR)
# or maintaining an out-of-tree component registration.
#
# Instead we use `prometheus.exporter.unix` cross-platform. This is
# Alloy's *embedded* node_exporter component — the node_exporter Go
# code is compiled into the alloy binary and exposed as a River
# component. It is NOT a separate node_exporter binary, package, or
# systemd unit; the AC's "no node_exporter dependency (no
# node_exporter added)" reads as forbidding a separate node_exporter
# service, and this satisfies that reading — nothing on the host
# except `alloy` runs.
#
# Platform coverage (default collector set):
#
#   * Linux: cpu, diskstats, filesystem, loadavg, meminfo, netdev,
#     hwmon (lm_sensors temperatures), thermal_zone, plus many
#     others.
#   * Darwin: boottime, cpu, diskstats, filesystem, loadavg,
#     meminfo, netdev, thermal, time, uname.
#
# We rely on the platform-appropriate defaults rather than pinning
# `set_collectors` so the exporter's platform-conditional collector
# registration handles the split naturally. One Darwin default is
# turned off again with `disable_collectors` — see the `thermal` note
# in the generated config below (issue #2765).
#
# ── Systemd unit health metrics (issue #2462, NixOS only) ───────────
#
# Collector choice: the embedded `prometheus.exporter.unix` `systemd`
# collector (same component as above), not a separate
# `otelcol.receiver.hostmetrics`-style route or a bundled
# node_exporter binary — same reasoning as the host-metrics decision
# above: it is already compiled in, so enabling it costs nothing
# extra on the host. `enable_collectors = ["systemd"]` turns it on
# (it is NOT part of the Linux default collector set, unlike the
# host-metrics collectors above); a `systemd { unit_include = ... }`
# block restricts which units it reports on, per the cardinality note
# below.
#
# The hard part: user units. The embedded systemd collector is the
# vendored `prometheus/node_exporter` collector, which talks to
# systemd over `github.com/coreos/go-systemd/v22/dbus`, calling
# `NewSystemConnectionContext` — a hardcoded dial of the *system* bus
# socket (`unix:path=/run/systemd/private`, confirmed by inspecting
# the built `alloy` binary's symbol table). There is no flag or River
# attribute to redirect it at a user session bus, and Alloy runs as a
# single system-wide DynamicUser service — it has no per-user session
# to dial into even if the collector supported it. So the embedded
# collector sees system units ONLY. The bespoke units this issue
# names (battery-monitor, flake-update-notifier, the prism sidecar)
# are `systemd.user.services` — invisible to this path.
#
# Resolution: a small user-scope timer (`systemdUserUnitTextfileTimer`
# below) runs `systemctl --user show --property=ActiveState` against
# the named user units and writes the result as Prometheus text
# exposition format into the directory the `textfile` collector reads
# (also part of the default Linux collector set, but a no-op until a
# `directory` is configured — done below). This reuses the same
# `prometheus.exporter.unix "node"` scrape already in the config; no
# second scrape target, no second `prometheus.remote_write`.
#
# The prism sidecar is NOT covered by either path: `tmux.nix` spawns
# it via `systemd-run --user --scope --collect` into an
# auto-named transient scope (`run-uN.scope`) precisely so its
# lifetime is decoupled from the `prism-restore.service` unit that
# creates it (issue #2340) — there is no stable, predictable unit
# name to point either collector at. The closest fixed-name proxy is
# `prism-restore.service` itself (the login-time unit that spawns
# sidecars), which IS tracked below; a unit-name allocation scheme
# for individual sidecars is out of scope for this PR — see the PR
# description for what it would take.
#
# Cardinality: see `systemdUnitIncludePattern` below for the
# allowlist and the resulting series-count estimate.
{
  config,
  lib,
  pkgs,
  isLinux,
  ...
}:
let
  cfg = config.nx.services.alloy;
  hostname = config.networking.hostName;
  isDarwin = !isLinux;

  # ── Syncthing scrape (issue #2461) ──────────────────────────────────
  #
  # Emitted on any host that runs Syncthing. `syncthing.enable` is the
  # only gate: a host without Syncthing must still evaluate, and must
  # not emit a scrape target that can never come up.
  #
  # Two earlier terms are gone. The `isLinux` term that #2461 carried
  # here went in #2697, once #2694 confirmed the Darwin Alloy path
  # pushes metrics. The pinned-key term went in #2787, which removed
  # the pinned REST API key and the GUI auth on all three hosts and
  # standardised on "localhost is the boundary" — Syncthing binds
  # 127.0.0.1:8384 and serves /metrics there with no credential, so
  # there is nothing left to gate on. See
  # modules/services/syncthing/secrets.nix for the full rationale.
  syncthingCfg = config.nx.services.syncthing;
  scrapeSyncthing = syncthingCfg.enable;

  # Syncthing's GUI/REST listen address, which is also where it serves
  # /metrics. On NixOS Syncthing is a system service; on Darwin it runs
  # under home-manager (modules/services/syncthing/default.nix), so the
  # same setting lives in the user's home-manager config. The `if` keeps
  # the NixOS-only option path from being read at all on Darwin. Note
  # that `services.syncthing` itself DOES resolve there -- it is a
  # `cert`/`key` submodule stub from modules/darwin/impermanence-stub.nix,
  # which secrets.nix writes into on both platforms -- but that stub
  # declares no `guiAddress`, so reading it on Darwin is an eval error.
  # The guard is load-bearing: do not remove it on the grounds that
  # `config.services.syncthing` evaluates.
  syncthingGuiAddress =
    if isLinux then
      config.services.syncthing.guiAddress
    else
      config.home-manager.users.${config.nx.username}.services.syncthing.guiAddress;

  # ── Prism exporter scrape (issue #2701) ──────────────────────────────
  #
  # On NixOS (navi, tui) the exporter is a user-scope systemd service,
  # running on the host only when a session is logged in (lingering is
  # not enabled — see the prism-exporter module header for why). Alloy
  # dials the loopback endpoint regardless of login state, so the
  # scrape target will report up=0 when no session exists. That is
  # expected behaviour and is accounted for in the AC for #2701.
  #
  # On Darwin (m4mac, #2705) the exporter is a launchd system daemon,
  # boot-started independent of any login, so this up=0-while-logged-
  # out gap does not apply there — see the prism-exporter module
  # header's "Darwin (#2705)" section for why.
  #
  # Gate on the exporter being enabled (system config). #2705 ported the
  # exporter to Darwin as a UserName-scoped launchd daemon (see
  # modules/services/prism-exporter/default.nix), listening on the same
  # loopback port on both platforms, so the scrape block below needs no
  # platform branch -- `isLinux` is intentionally absent here. Forward to
  # the existing prometheus.remote_write.
  prismExporterCfg = config.nx.services.prismExporter;
  scrapePrismExporter = prismExporterCfg.enable;

  # ── Systemd unit health metrics (issue #2462) ───────────────────────
  #
  # System-unit coverage: the embedded `prometheus.exporter.unix`
  # `systemd` collector, restricted to `.service` and `.timer` units.
  # Excluding `.mount`, `.device`, `.socket`, `.scope`, `.slice`, and
  # `.target` units cuts the bulk of the noise (mount points, kernel
  # devices, systemd-run scopes, cgroup slices) that would otherwise
  # multiply the series count for no operational value. Each surviving
  # unit is reported by `node_systemd_unit_state{name=...,state=...}`
  # with one series per (unit, state) pair — 6 possible ActiveState
  # values (active, reloading, inactive, failed, activating,
  # deactivating) — so the per-host series count is
  # `6 * count(.service + .timer units)`. A typical NixOS desktop
  # carries on the order of 100-130 service/timer units, so this is
  # roughly 600-800 series per host, ~1200-1600 for navi+tui combined.
  # See the PR description for the operator-measured actual count.
  systemdUnitIncludePattern = ".+\\.(service|timer)$";

  # Directory the textfile collector reads from, and the bespoke user
  # units written into it by `systemdUserUnitTextfileScript` below.
  # World-readable (0755 dir, 0644 files) so the alloy DynamicUser
  # service can read it — DynamicUser's implied ProtectSystem=strict
  # makes most of the filesystem read-only, not inaccessible, so a
  # world-readable path outside /usr, /boot, and /efi remains
  # readable without any extra grant.
  systemdUserUnitTextfileDir = "/var/lib/prometheus-node-exporter-textfile";
  systemdUserUnitTextfileName = "systemd-user-units.prom";

  # The bespoke user units named in issue #2462, plus the prism
  # exporter from #2701. `prism-restore` is the closest fixed-name
  # proxy for "the prism sidecar" — see the module header for why the
  # sidecar itself has no trackable unit name (it runs in an auto-named
  # transient scope, issue #2340).
  bespokeUserUnits = [
    "battery-monitor.service"
    "flake-update-notifier.service"
    "prism-exporter.service"
    "prism-restore.service"
  ];

  # Runs under the user's own session (via a `systemd.user.timer`), so
  # `systemctl --user` talks to the user's own session bus — this is
  # the resolution to the user-bus problem described in the module
  # header: proxy the read through a process that already has a user
  # session, rather than trying to make the system-scope alloy
  # service reach across the session boundary. Emits two low-
  # cardinality gauges per unit (active/failed booleans) rather than
  # mirroring the embedded collector's full 6-state pattern — 3 units
  # * 2 metrics = 6 series total, negligible next to the system-unit
  # count above.
  systemdUserUnitTextfileScript = pkgs.writeShellScript "systemd-user-unit-metrics" ''
    set -euo pipefail
    dir="${systemdUserUnitTextfileDir}"
    final="$dir/${systemdUserUnitTextfileName}"
    tmp="$final.$$"
    {
      echo "# HELP node_systemd_user_unit_active Whether a bespoke systemd --user unit is in the active state (1) or not (0)."
      echo "# TYPE node_systemd_user_unit_active gauge"
      echo "# HELP node_systemd_user_unit_failed Whether a bespoke systemd --user unit is in the failed state (1) or not (0)."
      echo "# TYPE node_systemd_user_unit_failed gauge"
      ${lib.concatMapStringsSep "\n      " (unit: ''
        state=$(${pkgs.systemd}/bin/systemctl --user show ${lib.escapeShellArg unit} --property=ActiveState --value 2>/dev/null || echo unknown)
        active=0; failed=0
        [ "$state" = active ] && active=1
        [ "$state" = failed ] && failed=1
        echo "node_systemd_user_unit_active{unit=\"${unit}\"} $active"
        echo "node_systemd_user_unit_failed{unit=\"${unit}\"} $failed"
      '') bespokeUserUnits}
    } > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv -f "$tmp" "$final"
  '';

  # Starts with a newline and ends without a blank line, so that the
  # empty case reproduces the previous file byte for byte.
  prismExporterScrapeBlock = lib.optionalString scrapePrismExporter ''

    // ── Prism exporter metrics ──────────────────────────────────────
    //
    // The exporter runs as a user-scope systemd service and serves
    // Prometheus metrics on a loopback port. See #2701 for the full
    // scope and #2700 for the exporter daemon shape.
    prometheus.scrape "prism_exporter" {
      targets         = [{"__address__" = "127.0.0.1:${toString prismExporterCfg.port}"}]
      metrics_path    = "/metrics"
      forward_to      = [prometheus.remote_write.default.receiver]
      scrape_interval = "60s"
    }
  '';

  # Starts with a newline and ends without a blank line, so that the
  # empty case reproduces the previous file byte for byte.
  #
  # The body below is deliberately platform-neutral: the generated
  # River is identical on navi, tui, and m4mac, differing only in the
  # interpolated GUI address.
  syncthingScrapeBlock = lib.optionalString scrapeSyncthing ''

    // ── Syncthing folder + connection metrics ───────────────────────
    //
    // Syncthing serves a Prometheus endpoint at /metrics on its REST
    // API port, which is bound to loopback (127.0.0.1:8384, set
    // explicitly in modules/services/syncthing/default.nix).
    //
    // No credential is sent. Issue #2787 removed the GUI auth and the
    // pinned REST API key on every host: with GUI auth off Syncthing
    // never installs its auth middleware on /metrics, so the Bearer
    // token this block used to carry was ignored anyway. Alloy dials
    // localhost and forwards over the authenticated remote_write
    // below, so no credential is needed here.
    prometheus.scrape "syncthing" {
      targets = [
        {"__address__" = "${syncthingGuiAddress}"},
      ]
      metrics_path    = "/metrics"
      forward_to      = [prometheus.remote_write.default.receiver]
      scrape_interval = "60s"
    }
  '';

  # Alloy River config. Kept as a single generated file for both
  # platforms — the metric-collection component is the same on both.
  #
  # NB: Alloy expects River syntax, not HCL. Backticks / dollar-braces
  # here are literal to River. Nix interpolation is via `${...}`; where
  # a literal `${...}` is needed in River, escape as `''${...}''`.
  alloyConfig = ''
    // Generated by modules/services/alloy/default.nix. Do not edit
    // /etc/alloy/config.alloy by hand — changes will be overwritten on
    // the next nixos-rebuild / darwin-rebuild switch.
    //
    // Host: ${hostname}
    // Push target: ${cfg.prometheusRemoteWriteUrl}

    // ── Host metrics (cross-platform) ────────────────────────────────
    //
    // prometheus.exporter.unix is Alloy's embedded node_exporter. On
    // Linux it enables the full default collector set (which includes
    // hwmon + thermal_zone for lm_sensors temperatures); on Darwin it
    // enables the Darwin-supported subset (cpu, diskstats, filesystem,
    // loadavg, meminfo, netdev, ...). This is a single component
    // compiled into the alloy binary — it is NOT a separate
    // node_exporter package or service (see module header for the
    // reasoning behind not using `otelcol.receiver.hostmetrics`).
    prometheus.exporter.unix "node" {${lib.optionalString isLinux ''

      // See module header "Systemd unit health metrics" for the
      // collector choice and the user-unit resolution.
      enable_collectors = ["systemd"]

      systemd {
        unit_include = `${systemdUnitIncludePattern}`
      }

      textfile {
        directory = "${systemdUserUnitTextfileDir}"
      }''}${lib.optionalString isDarwin ''

      // `thermal` is in the Darwin default collector set, but the
      // IOKit call it makes (IOPMCopyCPUPowerStatus) records nothing
      // on this hardware, so the collector fails on every scrape and
      // writes "no CPU power status has been recorded" into
      // /var/log/alloy.log. It produces no metric here, so turning it
      // off loses no data and keeps the log readable for the next
      // real fault. Name confirmed against node_exporter's
      // collector/thermal_darwin.go, which registers the collector as
      // `thermal` and carries that exact error string. See #2765.
      disable_collectors = ["thermal"]''}
    }

    prometheus.scrape "node" {
      targets         = prometheus.exporter.unix.node.targets
      forward_to      = [prometheus.remote_write.default.receiver]
      scrape_interval = "60s"
    }
    ${prismExporterScrapeBlock}
    ${syncthingScrapeBlock}
    // ── Push to home-ops Prometheus over the tailnet ────────────────
    //
    // v1 needs no auth token — the tailnet is the auth boundary
    // (headscale ACL grants `ben@` -> ts-metrics-ingest tcp/9090
    // only). If a token is ever required, add a
    //   basic_auth { username = "..." password_file = "..." }
    // block inside `endpoint` and wire the file via sops.
    //
    // external_labels adds a `hostname` label to every series so
    // navi / tui / m4mac are distinguishable in Prometheus.
    prometheus.remote_write "default" {
      external_labels = {
        hostname = "${hostname}",
      }

      endpoint {
        url = "${cfg.prometheusRemoteWriteUrl}"
      }
    }

    // ── Future seam: Loki log shipping ───────────────────────────────
    //
    // Deliberately not wired here (issue #2460 is metrics only). When
    // #2458's log-shipping child lands, add:
    //
    //   loki.write "default" {
    //     external_labels = { hostname = "${hostname}" }
    //     endpoint { url = "http://ts-loki.tailnet.internal:3100/loki/api/v1/push" }
    //   }
    //
    // and one or more loki.source.* components forwarding to it. The
    // headscale ACL already grants tcp/3100 to `ben@` so no ACL work
    // will be required at that time.
  '';
in
{
  options.nx.services.alloy = {
    enable = lib.mkEnableOption "Grafana Alloy telemetry collector (host metrics via prometheus.exporter.unix)";

    prometheusRemoteWriteUrl = lib.mkOption {
      type = lib.types.str;
      default = "http://ts-metrics-ingest.tailnet.internal:9090/api/v1/write";
      description = ''
        Prometheus `remote_write` endpoint URL that Alloy pushes host
        metrics to.

        Must be a MagicDNS name reachable from every enrolled node,
        NOT a LAN-only address — tui and m4mac need to keep pushing
        while roaming off the home LAN. The default resolves to the
        home-ops dedicated `ts-metrics-ingest` node (home-ops #3386),
        which is a single-replica ingest Prometheus with the
        remote-write receiver enabled. It is deliberately NOT
        `ts-prometheus.tailnet.internal`: that hostname is a
        2-replica HA scrape pair, and pushing remote_write at it
        would split samples non-deterministically across replicas.

        NOTE: as of PR time `ts-metrics-ingest.tailnet.internal` is
        not yet live (home-ops #3386 is gated behind their PR #3385).
        Until it lands, alloy's remote_write client will fail-retry
        against a non-resolving DNS name — the alloy service itself
        stays up (active/running); only the push side is deferred.
      '';
    };
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      # Config file at a stable path on both platforms so `alloy run`
      # can read it. On NixOS the upstream services.alloy module picks
      # this up via its reloadTriggers scan of `environment.etc."alloy/*.alloy"`.
      {
        environment.etc."alloy/config.alloy".text = alloyConfig;
      }

      # ── NixOS ────────────────────────────────────────────────────────
      #
      # Guarded by the `isLinux` specialArg from flake.nix rather than
      # `lib.mkIf pkgs.stdenv.hostPlatform.isLinux`. mkIf is lazy on its value but
      # the module system still walks the definition tree to register
      # option paths, and top-level `launchd` does not exist on NixOS
      # (nor does top-level `services.alloy` on nix-darwin). Using
      # mkIf would trip the option-existence check on the wrong
      # platform. `optionalAttrs` collapses to `{}` at Nix time before
      # the module system sees it, sidestepping the check. `isLinux`
      # comes via specialArgs so evaluating it does not require
      # resolving the module option set (which is what caused
      # infinite recursion when the same pattern was tried against
      # `pkgs.stdenv.hostPlatform.isDarwin`).
      (lib.optionalAttrs isLinux {
        services.alloy = {
          enable = true;
          # configPath defaults to /etc/alloy — matches the
          # environment.etc file above. `environmentFile` stays null:
          # v1 requires no auth token, and if it ever does, this is
          # the seam for a sops-decrypted path.
        };

        # World-readable so the alloy DynamicUser service can read the
        # textfile written into it -- see the module header "Systemd
        # unit health metrics" for why no group grant is needed here.
        #
        # Alloy needs no group grant for the Syncthing scrape either.
        # It used to join the group that owned the pinned REST API
        # key, because DynamicUser gives it no stable UID to grant the
        # file to; issue #2787 removed the key, so the group and the
        # grant are both gone.
        systemd.tmpfiles.rules = [
          "d ${systemdUserUnitTextfileDir} 0755 ${config.nx.username} users - -"
        ];

        # Periodic snapshot of the bespoke user units into the
        # textfile collector directory. Runs as the user (not root),
        # so `systemctl --user` reaches the caller's own session bus --
        # see the module header for why this is the resolution to the
        # embedded systemd collector's system-bus-only limitation.
        home-manager.users.${config.nx.username} = {
          systemd.user.services.systemd-user-unit-metrics = {
            Unit.Description = "Snapshot bespoke systemd --user unit health for Alloy's textfile collector";
            Service = {
              Type = "oneshot";
              ExecStart = "${systemdUserUnitTextfileScript}";
            };
          };

          systemd.user.timers.systemd-user-unit-metrics = {
            Unit.Description = "Periodic trigger for systemd-user-unit-metrics.service";
            Timer = {
              OnBootSec = "1m";
              OnUnitActiveSec = "1m";
            };
            Install.WantedBy = [ "timers.target" ];
          };
        };
      })

      # ── Darwin ───────────────────────────────────────────────────────
      #
      # nix-darwin has no first-class services.alloy, so we roll our
      # own launchd system daemon. Root-run so it can read every
      # node_exporter collector's data source without privilege drops.
      # RunAtLoad plus KeepAlive so it starts on boot and self-heals
      # if it crashes, without depending on a user login (unlike the
      # tailscale-headscale-up hook, which is a user agent because it
      # consumes sops user-scope secrets).
      (lib.optionalAttrs isDarwin {
        launchd.daemons.alloy = {
          serviceConfig = {
            # GODEBUG=netdns=cgo forces the cgo resolver at runtime.
            # Only effective because the overlay in overlays/default.nix
            # drops the `netgo` build tag on Darwin -- without that,
            # the cgo resolver isn't even compiled into the binary and
            # this flag is inert. See issue #2694: the pure-Go resolver
            # ignores macOS's scoped resolvers, so it cannot see the
            # tailscale-installed split-DNS route for tailnet.internal.
            EnvironmentVariables = {
              GODEBUG = "netdns=cgo";
            };
            ProgramArguments = [
              "/bin/sh"
              "-c"
              # /bin/wait4path guards against the daemon starting
              # before the /nix APFS volume is mounted at boot (the
              # alloy binary lives in the store).
              "/bin/wait4path ${pkgs.grafana-alloy}/bin/alloy && exec ${pkgs.grafana-alloy}/bin/alloy run --storage.path=/var/lib/alloy /etc/alloy/config.alloy"
            ];
            RunAtLoad = true;
            KeepAlive = true;
            # /var/log is root-writable and not a symlink hazard;
            # /tmp would be.
            StandardOutPath = "/var/log/alloy.log";
            StandardErrorPath = "/var/log/alloy.log";
            # Ensure the storage dir exists before alloy runs. launchd
            # will not create it for us.
            WorkingDirectory = "/var/lib/alloy";
          };
        };

        # Create the storage directory during activation. `alloy run
        # --storage.path` expects it to exist.
        system.activationScripts.extraActivation.text = lib.mkAfter ''
          echo "ensuring /var/lib/alloy exists for Grafana Alloy..."
          mkdir -p /var/lib/alloy
          chmod 0755 /var/lib/alloy
        '';
      })
    ]
  );
}
