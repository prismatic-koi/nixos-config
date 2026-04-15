{
  config,
  pkgs,
  lib,
  ...
}:
# This module is imported unconditionally but its config block is wrapped
# in the same mkIf guard as the rest of the prism module (see default.nix).
#
# On Linux, the prism-agent image is pulled from GHCR into the user's rootless
# podman store via a systemd user service that runs at every login. The pull is
# guarded so it is a no-op when the image is already present.
#
# On Darwin, podman runs inside a Linux VM managed by `podman machine`.
# A Home Manager LaunchAgent fires at login to:
#   1. Start the podman machine (guarded: checks state first and skips start
#      if already running, since `podman machine start` exits non-zero when the
#      VM is already up).
#   2. Pull the prism-agent image into the VM from GHCR (guarded: skipped when
#      the image is already present).
# Failures are captured in /tmp/podman-machine-start.log so they are visible
# for debugging; the prism fallback-to-host-mode mechanism (#472) handles the
# case where the VM is unavailable at spawn time.
let
  username = config.nx.username;

  image = "ghcr.io/prismatic-koi/prism-agent:latest";
in
{
  config = lib.mkIf config.nx.programs.prism.enable (
    lib.mkMerge [
      # ── Linux ────────────────────────────────────────────────────────────────
      # Pull the image from GHCR via a systemd user service at every login.
      # podman.enable is included in the guard so the service is skipped
      # entirely when the user has explicitly disabled podman.
      (lib.mkIf (pkgs.stdenv.isLinux && config.nx.programs.podman.enable) {
        # Pull the prism-agent container image from GHCR into the user's podman
        # store on login via a systemd user service.
        #
        # The service is declared as oneshot with RemainAfterExit=yes: systemd
        # considers it "active" once ExecStart returns successfully and will NOT
        # re-run it within the same login session, satisfying the idempotency
        # requirement.  On the next login (e.g. after a reboot) the unit is in
        # the "inactive" state again and will re-run.
        #
        # Idempotency: the script checks `podman image exists` first and skips
        # the pull when the image is already present.
        home-manager.users.${username} = {
          home.persistence."/persist" = {
            directories = [
              ".local/share/containers"
            ];
          };
          systemd.user.services.prism-agent-image = {
            Unit = {
              Description = "Pull prism-agent container image from GHCR into rootless podman storage";
              # Wait for the network to be ready before attempting the pull.
              After = [ "network-online.target" ];
              Wants = [ "network-online.target" ];
              # Run before prism session restore so the image is available when
              # the first container spawn happens.
              Before = [ "prism-restore.service" ];
            };
            Service = {
              Type = "oneshot";
              RemainAfterExit = true;
              ExecStart =
                let
                  script = pkgs.writeShellScript "pull-prism-agent-image" ''
                    PODMAN="${pkgs.podman}/bin/podman"

                    if $PODMAN image exists ${image}; then
                      echo "prism: ${image} already present, skipping pull." >&2
                    else
                      echo "prism: pulling ${image}..." >&2
                      $PODMAN pull ${image}
                    fi
                  '';
                in
                script;
            };
            Install = {
              WantedBy = [ "default.target" ];
            };
          };
        };
      })

      # ── Darwin ───────────────────────────────────────────────────────────────
      # Start the podman machine VM and pull the image at login via a
      # Home Manager LaunchAgent.  Home Manager writes the plist to
      # ~/Library/LaunchAgents/ and loads/unloads it automatically;
      # after `darwin-rebuild switch` the plist is updated in place.
      # Note: we do not gate on `config.nx.programs.podman.enable` here.
      # On Darwin that option is set to `false` in m4mac because
      # `virtualisation.podman` is Linux-only; podman is instead installed via
      # `environment.systemPackages`.  The LaunchAgent is the Darwin equivalent
      # of the Linux service, and its purpose is precisely to set up
      # the VM — it should run unconditionally on any Darwin host that has prism
      # enabled.
      (lib.mkIf pkgs.stdenv.isDarwin (
        let
          # Shell script that starts the podman machine then pulls the prism-agent
          # image into it.  Written as a separate derivation so the LaunchAgent
          # ProgramArguments array can reference it directly.
          #
          # Design notes:
          #   - `podman machine start` exits non-zero (exit 125) if the VM is already
          #     running.  We check state explicitly via `podman machine inspect` before
          #     calling start so that a running VM is treated as a success, not a failure.
          #   - We pull the image inside the VM via `podman machine ssh -- podman pull`
          #     rather than calling `podman pull` directly on the host, because on Darwin
          #     `podman pull` routes through the machine socket and the image must reside
          #     inside the Linux VM.
          #   - All output goes to stdout/stderr which the LaunchAgent captures in
          #     StandardOutPath / StandardErrorPath below.
          podmanMachineStartScript = pkgs.writeShellScript "podman-machine-start" ''
            set -euo pipefail

            PODMAN="${pkgs.podman}/bin/podman"

            echo "podman-machine-start: starting podman machine..." >&2

            # Check if the machine is already running to make the start step idempotent.
            # `podman machine inspect` outputs JSON; we check the State field.
            # If the machine doesn't exist yet, inspect will fail — treat that as
            # "not running" so we fall through to `machine start`.
            machine_state=$("$PODMAN" machine inspect --format '{{.State}}' 2>/dev/null || true)

            if [ "$machine_state" = "running" ]; then
              echo "podman-machine-start: machine is already running, skipping start." >&2
            else
              "$PODMAN" machine start || {
                echo "podman-machine-start: 'podman machine start' failed, aborting image pull." >&2
                exit 1
              }
            fi

            echo "podman-machine-start: checking prism-agent image in VM..." >&2
            if "$PODMAN" machine ssh -- podman image exists ${image}; then
              echo "podman-machine-start: ${image} already present in VM, skipping pull." >&2
            else
              echo "podman-machine-start: pulling ${image} into VM..." >&2
              "$PODMAN" machine ssh -- podman pull ${image}
              echo "podman-machine-start: ${image} pulled successfully." >&2
            fi
          '';
        in
        {
          home-manager.users.${username}.launchd.agents.podman-machine-start = {
            enable = true;
            config = {
              ProgramArguments = [ "${podmanMachineStartScript}" ];
              # Fire once at login.  KeepAlive is intentionally omitted so the
              # agent does not restart on failure — a broken VM should not create
              # a restart loop.
              RunAtLoad = true;
              # AbandonProcessGroup allows the podman VM processes (vfkit,
              # gvproxy) to survive after the startup script exits.  Without
              # this, launchd kills the entire process group on job completion,
              # tearing down the VM immediately after it starts.
              AbandonProcessGroup = true;
              # Capture output for debugging; visible via:
              #   tail -f /tmp/podman-machine-start.log
              StandardOutPath = "/tmp/podman-machine-start.log";
              StandardErrorPath = "/tmp/podman-machine-start.log";
            };
          };
        }
      ))
    ]
  );
}
