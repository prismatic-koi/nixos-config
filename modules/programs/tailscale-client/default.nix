# Tailscale client — enrols the host in the home-ops headscale tailnet.
#
# Part of the fleet telemetry train (issues #2458 / #2459). This module is
# the CLIENT-side seam: the control server, subnet router, and ACL policy
# (which grants reach into prometheus:9090 and loki:3100) live in the
# home-ops repo and are already deployed at PR time. Here we only enrol
# navi, tui, and m4mac as tailnet nodes. Nodes enrol untagged; see #2467.
#
# Notes on wgnord: `modules/programs/wgnord.nix` is the NordVPN exit
# client — completely unrelated to this module. We mirror its structural
# shape (a networking-client module with secrets sourced from sops) but
# not its behaviour.
#
# ── Runtime wiring rationale ─────────────────────────────────────────────
#
# Two things enter this module from sops at runtime:
#
#   * `login_server` — the headscale control-plane URL, e.g.
#     `https://hs.<domain>`. Treated as SECRET by explicit choice: the URL
#     identifies the control plane and is not something we want visible in
#     `/nix/store`.
#   * `preauth_<hostname>` — a per-host preauth key minted from headscale.
#     Consumed once at first enrolment; the local node key persists after
#     that and this file is not re-read on subsequent boots.
#
# The stock `services.tailscale` NixOS module offers two integration
# points:
#
#   * `services.tailscale.authKeyFile` — a path read at runtime.
#     Store-safe (only the path is baked in, the file's contents stay on
#     disk).
#   * `services.tailscale.extraUpFlags` — a list of literal shell tokens
#     that lands inside the generated `tailscaled-autoconnect.service`
#     unit under `/nix/store`. World-readable.
#
# Passing the login-server URL via `extraUpFlags` therefore violates the
# "no secrets in the store" acceptance criterion. And passing it as
# `--login-server="$(cat /run/secrets/...)"` is worse: the wrapper
# escapes each flag via `escapeShellArgs`, so shell interpolation is
# quoted literal — the flag would arrive at `tailscale up` verbatim as
# the string `$(cat /run/secrets/...)` and enrolment would fail.
#
# So we bypass the stock autoconnect entirely and write our own
# systemd/launchd hook that reads BOTH files at runtime, then runs:
#
#   tailscale up \
#     --login-server="$(cat <url-file>)" \
#     --auth-key="$(cat <key-file>)" \
#     --accept-routes
#
# `--accept-routes` is non-secret and hard-coded in the unit. The
# state-loop pattern is lifted from the built-in
# `tailscaled-autoconnect.service` in nixpkgs — we only run
# `tailscale up` when BackendState is `NeedsLogin`, `NeedsMachineAuth`,
# or `Stopped`, and exit cleanly (via `systemd-notify --ready` on Linux)
# when it reaches `Running`. Idempotent across reboots: once the node is
# registered, the hook observes `Running` and exits without touching
# either secret.
#
# ── Darwin (m4mac) mechanism — chosen and documented ─────────────────────
#
# Three candidates were considered for the Darwin client:
#
#   1. nix-darwin's `services.tailscale` (upstream open-source
#      `tailscaled` from nixpkgs, launched as a launchd system daemon).
#   2. Homebrew cask `tailscale` (the Mac App Store bundle wrapped as a
#      cask — closed-source GUI, no first-class custom-`--login-server`
#      flag, would need to be driven via the GUI or its bundled CLI
#      shim).
#   3. MAS App Store `tailscale` — same closed-source bundle as (2),
#      with the same headscale-hostility.
#
# We use (1) — nix-darwin's `services.tailscale`. Rationale:
#
#   * It uses the same upstream `tailscaled` binary from nixpkgs as the
#     NixOS module, so the CLI surface (`tailscale up --login-server=...
#     --auth-key=...`) is identical across all three hosts.
#   * The App Store / Homebrew builds of Tailscale ship a Network
#     Extension wrapper that Tailscale (the company) has intentionally
#     left without a documented custom-control-server hook — headscale
#     is not officially supported there.
#   * It keeps the module surface homogeneous — one `services.tailscale`
#     option on both platforms, one `tailscale` CLI on both platforms,
#     one shell script running the same `tailscale up` state loop.
#
# The Darwin enrolment hook is a home-manager launchd USER agent (not a
# system daemon). Two reasons:
#
#   * The user-scope sops secrets are decrypted by the sops-nix
#     home-manager launchd user agent, which only fires after the user
#     logs in. A system daemon firing at boot would race the secret
#     decryption. A user agent naturally sequences after login.
#   * `tailscale` CLI needs to reach `/var/run/tailscaled.socket` which
#     is root-owned; the user agent uses `sudo -n` (m4mac already has
#     `NOPASSWD: ALL` for the primary user, see machines/m4mac/
#     configuration.nix) to elevate. `sudo -n` is fail-fast — if
#     NOPASSWD is ever removed, the agent errors out immediately rather
#     than blocking on a password prompt.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.nx.programs.tailscaleClient;
  username = config.nx.username;
  hostname = config.networking.hostName;
  sopsFile = ./secrets/tailscale.sops.yaml;

  # Names of the sops keys inside the yaml file. Nested access via `/`
  # matches the layout of ssh.sops.yaml elsewhere in this repo.
  loginServerKey = "tailscale/login_server";
  preauthKey = "tailscale/preauth_${hostname}";

  # Common baseline flags. These are non-secret and safe to bake into
  # the unit script (i.e. visible in /nix/store).
  baselineFlags = [
    "--accept-routes"
  ];
  baselineFlagsStr = lib.concatStringsSep " " baselineFlags;
in
{
  options.nx.programs.tailscaleClient = {
    enable = lib.mkEnableOption "tailscale client enrolled against the home-ops headscale tailnet";
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      # ── NixOS ────────────────────────────────────────────────────────
      (lib.mkIf pkgs.stdenv.isLinux {
        # System-scope sops secrets. Root-owned (0400 by default) since
        # they are consumed by the root-run tailscale-headscale-up unit.
        sops.secrets."${loginServerKey}" = {
          inherit sopsFile;
        };
        sops.secrets."${preauthKey}" = {
          inherit sopsFile;
        };

        # Start tailscaled. We do NOT set authKeyFile — that would
        # trigger the stock `tailscaled-autoconnect.service`, which
        # calls `tailscale up` without a `--login-server` flag and
        # thus contacts login.tailscale.com instead of our headscale
        # control plane.
        services.tailscale.enable = true;

        # `--accept-routes` on the client makes the kernel install
        # routes for the tailnet-advertised subnets (here: 10.43.0.0/16,
        # the k3s service CIDR advertised by the home-ops subnet
        # router). Loose reverse-path filtering is the tailscale-
        # recommended pairing so return traffic from those subnets is
        # not dropped by rp_filter=strict on the client's physical
        # interface. Equivalent to `services.tailscale.useRoutingFeatures
        # = "client";` but set directly to keep this module
        # cross-platform (the useRoutingFeatures option exists only in
        # the NixOS module, not the nix-darwin module).
        networking.firewall.checkReversePath = "loose";

        # Persist tailscaled state across reboots on impermanent hosts.
        # navi and tui both wipe the root btrfs subvolume every boot
        # (see modules/system/impermanence.nix — nx.system.impermanence
        # defaults to enabled on Linux). Without persistence, every
        # reboot re-hits the state loop's `NeedsLogin` branch and:
        #
        #   * with a single-use preauth key, enrolment fails after the
        #     first boot (the key was consumed); or
        #   * with a reusable preauth key, the host registers as a new
        #     duplicate node (navi, navi-1, navi-2, …) with a fresh
        #     tailnet IP, breaking MagicDNS and any Prometheus target
        #     list keyed on the tailnet address.
        #
        # tailscaled's systemd unit sets `StateDirectory=tailscale`, so
        # the persistent files live at /var/lib/tailscale/ (chiefly
        # tailscaled.state, which holds the node identity keys).
        # Mirroring the wgnord and libvirt precedents elsewhere in
        # this repo.
        environment.persistence."/persist/system" = {
          directories = [
            "/var/lib/tailscale"
          ];
        };

        # Custom autoconnect unit — reads both secrets at runtime and
        # runs `tailscale up` against headscale. Lifted from the state
        # loop in nixpkgs' tailscaled-autoconnect.service.
        systemd.services.tailscale-headscale-up = {
          description = "Enrol tailscale client against the home-ops headscale tailnet";
          after = [ "tailscaled.service" ];
          wants = [ "tailscaled.service" ];
          wantedBy = [ "multi-user.target" ];
          serviceConfig = {
            Type = "notify";
            # If tailscale up transiently fails (e.g. headscale not yet
            # reachable at boot on a slow link), retry rather than
            # marking the unit as failed and preventing later recovery.
            Restart = "on-failure";
            RestartSec = "10s";
          };
          path = [
            config.services.tailscale.package
            pkgs.jq
          ];
          enableStrictShellChecks = true;
          script = ''
            getState() {
              tailscale status --json --peers=false | jq -r '.BackendState'
            }

            lastState=""
            while state="$(getState)"; do
              if [[ "$state" != "$lastState" ]]; then
                # See ipn/backend.go for the enum values.
                case "$state" in
                  NeedsLogin|NeedsMachineAuth|Stopped)
                    echo "State = $state — running tailscale up against headscale"
                    tailscale up \
                      --login-server="$(cat ${config.sops.secrets."${loginServerKey}".path})" \
                      --auth-key="$(cat ${config.sops.secrets."${preauthKey}".path})" \
                      ${baselineFlagsStr}
                    ;;
                  Running)
                    echo "Tailscale is running"
                    systemd-notify --ready
                    exit 0
                    ;;
                  *)
                    echo "State = $state — waiting for Running or systemd timeout"
                    ;;
                esac
              fi
              lastState="$state"
              sleep .5
            done
          '';
        };
      })

      # ── Darwin ───────────────────────────────────────────────────────
      (lib.mkIf pkgs.stdenv.isDarwin {
        # nix-darwin's services.tailscale runs `tailscaled` as a launchd
        # system daemon. No authKeyFile / extraUpFlags support here —
        # everything is done by the user agent below.
        services.tailscale.enable = true;

        home-manager.users.${username} = {
          # User-scope sops secrets. sops-nix's home-manager module
          # decrypts these on user login via a launchd user agent, so
          # they are guaranteed to be present by the time the agent
          # below fires (both are RunAtLoad user agents; the ordering
          # is via a wait-for-readable-file loop in the enrolment
          # script, not launchd DependentServices, because launchd
          # user-agent ordering is loose).
          sops.secrets."${loginServerKey}" = {
            inherit sopsFile;
          };
          sops.secrets."${preauthKey}" = {
            inherit sopsFile;
          };

          launchd.agents.tailscale-headscale-up = {
            enable = true;
            config = {
              ProgramArguments = [
                (pkgs.writeShellScript "tailscale-headscale-up" ''
                  set -eu

                  TS="${config.services.tailscale.package}/bin/tailscale"
                  JQ="${pkgs.jq}/bin/jq"
                  LOGIN_SERVER_FILE="${config.home-manager.users.${username}.sops.secrets."${loginServerKey}".path}"
                  AUTHKEY_FILE="${config.home-manager.users.${username}.sops.secrets."${preauthKey}".path}"

                  # Wait for the sops secrets to exist. sops-nix user
                  # agent decrypts on login; this loop tolerates being
                  # scheduled before that. Bounded at 60 iterations
                  # (~2 min) so we don't spin forever on a broken
                  # sops setup.
                  for _ in $(seq 1 60); do
                    if [ -r "$LOGIN_SERVER_FILE" ] && [ -r "$AUTHKEY_FILE" ]; then
                      break
                    fi
                    sleep 2
                  done

                  # Wait for tailscaled to be reachable. The launchd
                  # daemon may be starting concurrently.
                  for _ in $(seq 1 60); do
                    if sudo -n "$TS" status --json --peers=false >/dev/null 2>&1; then
                      break
                    fi
                    sleep 2
                  done

                  getState() {
                    sudo -n "$TS" status --json --peers=false | "$JQ" -r '.BackendState'
                  }

                  lastState=""
                  while state="$(getState)"; do
                    if [ "$state" != "$lastState" ]; then
                      case "$state" in
                        NeedsLogin|NeedsMachineAuth|Stopped)
                          echo "State = $state — running tailscale up against headscale"
                          sudo -n "$TS" up \
                            --login-server="$(cat "$LOGIN_SERVER_FILE")" \
                            --auth-key="$(cat "$AUTHKEY_FILE")" \
                            ${baselineFlagsStr}
                          ;;
                        Running)
                          echo "Tailscale is running"
                          exit 0
                          ;;
                        *)
                          echo "State = $state — waiting for Running"
                          ;;
                      esac
                    fi
                    lastState="$state"
                    sleep 1
                  done
                '').outPath
              ];
              RunAtLoad = true;
              # Do NOT set KeepAlive = true. This is a one-shot
              # enrolment; once the state loop sees Running it exits 0
              # and we want launchd to leave it alone until the next
              # login. If enrolment fails, the launchd exit-code
              # handling logs the failure but does not busy-restart.
              KeepAlive = false;
              StandardOutPath = "/tmp/tailscale-headscale-up.log";
              StandardErrorPath = "/tmp/tailscale-headscale-up.log";
            };
          };
        };
      })
    ]
  );
}
