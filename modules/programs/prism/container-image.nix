{
  config,
  pkgs,
  lib,
  ...
}:
# This module is imported unconditionally but its config block is wrapped
# in the same mkIf guard as the rest of the prism module (see default.nix).
#
# On Linux, the prism-agent image is loaded into the user's rootless podman
# store via a systemd user service that runs at every login.
#
# On Darwin, podman runs inside a Linux VM managed by `podman machine`.
# A Home Manager LaunchAgent fires at login to:
#   1. Start the podman machine (guarded: checks state first and skips start
#      if already running, since `podman machine start` exits non-zero when the
#      VM is already up).
#   2. Load the prism-agent image into the VM.
# Failures are captured in /tmp/podman-machine-start.log so they are visible
# for debugging; the prism fallback-to-host-mode mechanism (#472) handles the
# case where the VM is unavailable at spawn time.
let
  username = config.nx.username;

  # Ubuntu 24.04 LTS base image pulled at evaluation time.
  # Re-run nix-prefetch-docker to refresh when the upstream image changes:
  #   nix run nixpkgs#nix-prefetch-docker -- ubuntu 24.04 --os linux --arch amd64
  ubuntuBase = pkgs.dockerTools.pullImage {
    imageName = "ubuntu";
    imageDigest = "sha256:186072bba1b2f436cbb91ef2567abca677337cfc786c86e107d25b7072feef0c";
    hash = "sha256-8DXLXnojKTuXpneMIHCEvcqJRHyA4dAKmMYE36QGMMU=";
    finalImageName = "ubuntu";
    finalImageTag = "24.04";
  };

  # The prism agent container image.
  # Built with buildLayeredImage so each Nix package gets its own layer —
  # podman can cache unchanged layers and only reload what changed.
  #
  # `contents` accepts a list of derivations to copy to the image root.
  # Wrapping packages in buildEnv merges them into a single /bin, /lib,
  # etc. symlink farm so binaries land on PATH. Passing raw package
  # derivations directly would place them at their Nix store paths only,
  # leaving them unreachable via PATH.
  prismAgentImage = pkgs.dockerTools.buildLayeredImage {
    name = "prism-agent";
    tag = "latest";

    # Ubuntu 24.04 base provides apt, standard system libs, and a familiar
    # runtime environment.  Agents may apt-get install additional tools at
    # runtime; those changes are discarded with the container.
    fromImage = ubuntuBase;

    contents = [
      # CA certificates — places bundles at /etc/ssl/certs/ca-bundle.crt and
      # /etc/ssl/certs/ca-certificates.crt so curl, git, gh, and opencode all
      # find them without needing env var overrides.
      pkgs.dockerTools.caCertificates

      (pkgs.buildEnv {
        name = "prism-agent-root";
        paths = with pkgs; [
          # Shell and core utilities — pin the Nix versions so PATH is consistent
          # regardless of what Ubuntu provides
          bash
          coreutils

          # Development tools
          opencode
          claude-code
          git
          openssh # git push/fetch over SSH remotes
          gh
          go
          gcc # C compiler — required for cgo
          nodejs # LTS Node.js for JavaScript/TypeScript tooling
          bun # Fast JavaScript runtime / bundler
          jq
          yq-go
          ripgrep
          fd
          curl
          wget
          unzip
          sqlite # prism uses SQLite; agents may query it directly
          python3 # scripting, data processing, quick automation

          # Browser automation — playwright-cli wraps chromium
          playwright-cli

          # Prism CLI — agents need prism spawn, prompt, checkin, etc.
          prism

          # Cloud and infrastructure
          awscli2
          opentofu
          kubectl
          kubernetes-helm
          fluxcd

          # Secrets tooling — agents may read or edit sops-encrypted files
          sops
          age

          # Per-project environment management
          direnv

          # Nix toolchain — agents run nix build/eval/flake check and nixfmt
          nix
          nixfmt

          # cacert — Nix itself needs NIX_SSL_CERT_FILE to point at the bundle.
          # dockerTools.caCertificates (added as a top-level content entry below)
          # handles the /etc/ssl/certs layout that other tools expect.
          cacert

          # LSP servers
          gopls
          typescript-language-server
          nil # Nix LSP
        ];
        pathsToLink = [
          "/bin"
          "/lib"
          "/share"
          "/etc"
        ];
      })
    ];

    extraCommands = ''
      # Nix experimental features — required for nix build, nix flake, etc.
      mkdir -p etc/nix
      echo "experimental-features = nix-command flakes" > etc/nix/nix.conf

      # Nix wrapper: when the host's nix daemon socket is mounted (by
      # container.go), transparently inject --eval-store auto so that
      # "nix build", "nix flake check", etc. evaluate locally (can see
      # /workspace) but delegate store operations to the host daemon
      # (reusing cached derivations). Without the socket the wrapper is
      # a no-op passthrough to the real nix binary.
      real_nix=$(readlink -f bin/nix)
      mv bin/nix bin/.nix-real
      cat > bin/nix << 'WRAPPER'
      #!/bin/bash
      SOCKET=/nix/var/nix/daemon-socket/socket
      if [ -S "$SOCKET" ]; then
        # Subcommands that accept --eval-store
        case "''${1:-}" in
          build|eval|flake|develop|path-info|print-dev-env|log|derivation)
            exec /bin/.nix-real "$1" --eval-store auto "''${@:2}"
            ;;
        esac
      fi
      exec /bin/.nix-real "$@"
      WRAPPER
      chmod +x bin/nix
    '';

    config = {
      Cmd = [ "/bin/bash" ];
      Env = [
        # /bin first so Nix-installed tools shadow Ubuntu's equivalents
        "PATH=/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
        # CA bundle — used by Nix itself (other tools find certs via the
        # standard /etc/ssl/certs/ paths placed by dockerTools.caCertificates).
        "NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
      ];
    };
  };

in
{
  config = lib.mkIf config.nx.programs.prism.enable (
    lib.mkMerge [
      # ── Linux ────────────────────────────────────────────────────────────────
      # Load the image via a systemd user service at every login.
      # podman.enable is included in the guard so the service is skipped
      # entirely when the user has explicitly disabled podman.
      (lib.mkIf (pkgs.stdenv.isLinux && config.nx.programs.podman.enable) {
        # Load the prism-agent container image into the user's podman store on
        # login via a systemd user service.
        #
        # This replaces the former nixos-rebuild activation script, which only ran
        # at switch time.  ~/.local/share/containers/ is now persisted by
        # impermanence, so the image survives reboots and only needs reloading
        # when the image definition changes (incremental, layer-cached).
        #
        # The service is declared as oneshot with RemainAfterExit=yes: systemd
        # considers it "active" once ExecStart returns successfully and will NOT
        # re-run it within the same login session, satisfying the idempotency
        # requirement.  On the next login (e.g. after a reboot) the unit is in
        # the "inactive" state again and will re-run.
        #
        # Idempotency:
        #   1. Remove the existing tag first — prevents the old manifest becoming a
        #      dangling <none>:<none> image when the tag is moved to the new image.
        #   2. Prune dangling images after load — cleans up any layers that were
        #      previously unreferenced (e.g. from an interrupted switch).
        #   Both commands use `|| true` so a missing image or empty prune is not fatal.
        home-manager.users.${username} = {
          home.persistence."/persist" = {
            directories = [
              ".local/share/containers"
            ];
          };
          systemd.user.services.prism-agent-image = {
            Unit = {
              Description = "Load prism-agent container image into rootless podman storage";
              # Run before prism session restore so the image is available when
              # the first container spawn happens.
              Before = [ "prism-restore.service" ];
            };
            Service = {
              Type = "oneshot";
              RemainAfterExit = true;
              ExecStart =
                let
                  # The expected image ID is derived from the Nix store path at
                  # build time. podman load prints "Loaded image: <id>" on the
                  # last line — we extract the sha256 digest from the archive
                  # manifest so we can skip the load when the image is unchanged.
                  script = pkgs.writeShellScript "load-prism-agent-image" ''
                    PODMAN="${pkgs.podman}/bin/podman"

                    # Extract the expected image ID from the archive manifest.
                    EXPECTED_ID=$(${pkgs.gnutar}/bin/tar -xf ${prismAgentImage} --to-stdout manifest.json 2>/dev/null \
                      | ${pkgs.jq}/bin/jq -r '.[0].Config // empty' 2>/dev/null \
                      | ${pkgs.gnused}/bin/sed 's/\.json$//')
                    CURRENT_ID=$($PODMAN image inspect prism-agent:latest --format '{{.Id}}' 2>/dev/null || true)

                    if [ -n "$EXPECTED_ID" ] && [ "$CURRENT_ID" = "$EXPECTED_ID" ]; then
                      echo "prism: prism-agent:latest is up to date ($EXPECTED_ID), skipping load." >&2
                      exit 0
                    fi

                    echo "prism: loading prism-agent:latest into podman..." >&2
                    $PODMAN image rm prism-agent:latest 2>/dev/null || true
                    $PODMAN load < ${prismAgentImage}
                    $PODMAN image prune --force 2>/dev/null || true
                    echo "prism: prism-agent:latest loaded successfully." >&2
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
      # Start the podman machine VM and load the image at login via a
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
          # Shell script that starts the podman machine then loads the prism-agent
          # image into it.  Written as a separate derivation so the LaunchAgent
          # ProgramArguments array can reference it directly.
          #
          # Design notes:
          #   - `podman machine start` exits non-zero (exit 125) if the VM is already
          #     running.  We check state explicitly via `podman machine inspect` before
          #     calling start so that a running VM is treated as a success, not a failure.
          #   - We pipe the image tarball through `podman machine ssh` rather than
          #     calling `podman load` directly, because on Darwin `podman load` routes
          #     through the socket and cannot stream from stdin reliably at login time.
          #     `podman machine ssh -- podman load` runs inside the VM where stdin is
          #     a proper pipe.
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
                echo "podman-machine-start: 'podman machine start' failed, aborting image load." >&2
                exit 1
              }
            fi

            echo "podman-machine-start: loading prism-agent image into VM..." >&2
            # Remove the old tag first so it does not become a dangling <none>:<none>
            # after the new image is loaded.
            "$PODMAN" machine ssh -- podman image rm prism-agent:latest 2>/dev/null || true
            # Stream the image tarball into the VM via podman machine ssh.
            "$PODMAN" machine ssh -- podman load < ${prismAgentImage}
            "$PODMAN" machine ssh -- podman image prune --force 2>/dev/null || true

            echo "podman-machine-start: prism-agent:latest loaded successfully." >&2
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
