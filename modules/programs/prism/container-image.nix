{
  config,
  pkgs,
  lib,
  ...
}:
{
  # This module is imported unconditionally but its config block is wrapped
  # in the same mkIf guard as the rest of the prism module (see default.nix).
  # On Darwin, podman runs inside a Linux VM managed by `podman machine`.
  # Automatically loading the image into the VM at activation time is not
  # straightforward without assuming `podman machine` is already running,
  # so we skip the activation step on Darwin and document the manual workflow.
  # podman.enable is required because the activation script uses the podman
  # store path directly — if podman is not enabled there is no guarantee the
  # store path exists at runtime.
  config =
    lib.mkIf
      (config.nx.programs.prism.enable && pkgs.stdenv.isLinux && config.nx.programs.podman.enable)
      (
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
              (pkgs.buildEnv {
                name = "prism-agent-root";
                paths = with pkgs; [
                  # Shell and core utilities — pin the Nix versions so PATH is consistent
                  # regardless of what Ubuntu provides
                  bash
                  coreutils

                  # Development tools
                  opencode
                  git
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

            config = {
              Cmd = [ "/bin/bash" ];
              Env = [
                # /bin first so Nix-installed tools shadow Ubuntu's equivalents
                "PATH=/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
              ];
            };
          };
        in
        {
          # Load the prism-agent container image into the user's podman store on
          # every nixos-rebuild switch.
          #
          # Runs as the prism user (not root) because podman is rootless: activation
          # scripts run as root, so we use `sudo -u <user>` to target the correct
          # per-user container storage (~/.local/share/containers).
          #
          # Idempotency:
          #   1. Remove the existing tag first — prevents the old manifest becoming a
          #      dangling <none>:<none> image when the tag is moved to the new image.
          #   2. Prune dangling images after load — cleans up any layers that were
          #      previously unreferenced (e.g. from an interrupted switch).
          #   Both commands use `|| true` so a missing image or empty prune is not fatal.
          #
          # Darwin note: this activation script is Linux-only. On macOS, podman runs
          # inside a Linux VM (`podman machine`). To load the image manually after
          # a rebuild, run:
          #   podman machine start && podman load < ${prismAgentImage}
          #
          # Activation scripts run in a minimal environment without the usual PATH,
          # so podman is referenced by its full Nix store path rather than a bare
          # command name.
          system.activationScripts.prismAgentContainerImage = ''
            echo "prism: loading prism-agent:latest into podman (user: ${username})..." >&2
            sudo -u ${username} ${pkgs.podman}/bin/podman image rm prism-agent:latest 2>/dev/null || true
            sudo -u ${username} ${pkgs.podman}/bin/podman load < ${prismAgentImage}
            sudo -u ${username} ${pkgs.podman}/bin/podman image prune --force 2>/dev/null || true
            echo "prism: prism-agent:latest loaded successfully." >&2
          '';
        }
      );
}
