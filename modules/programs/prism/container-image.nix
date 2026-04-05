{
  config,
  pkgs,
  lib,
  ...
}:
let
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
  prismAgentImage = pkgs.dockerTools.buildLayeredImage {
    name = "prism-agent";
    tag = "latest";

    # Ubuntu 24.04 base provides apt, standard system libs, and a familiar
    # runtime environment.  Agents may apt-get install additional tools at
    # runtime; those changes are discarded with the container.
    fromImage = ubuntuBase;

    contents = with pkgs; [
      # Development tools
      opencode
      git
      gh
      go
      nodejs # LTS Node.js for JavaScript/TypeScript tooling
      bun # Fast JavaScript runtime / bundler
      jq
      ripgrep
      fd
      curl

      # LSP servers
      gopls
      typescript-language-server
      nil # Nix LSP
    ];

    config = {
      Cmd = [ "/bin/bash" ];
      Env = [
        # Ensure Nix-installed binaries are on PATH inside the container
        "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
      ];
    };
  };

in
{
  # This module is imported unconditionally but its config block is wrapped
  # in the same mkIf guard as the rest of the prism module (see default.nix).
  # On Darwin, podman runs inside a Linux VM managed by `podman machine`.
  # Automatically loading the image into the VM at activation time is not
  # straightforward without assuming `podman machine` is already running,
  # so we skip the activation step on Darwin and document the manual workflow.
  config = lib.mkIf (config.nx.programs.prism.enable && pkgs.stdenv.isLinux) {
    # Load the prism-agent container image into podman on every nixos-rebuild switch.
    #
    # Idempotency: the existing tag is removed before loading so that the
    # previous manifest never becomes a dangling/orphaned image.  If no image
    # exists yet, `podman image rm` exits 0 via `|| true` and the load
    # proceeds normally.  Running switch twice with no input changes is
    # therefore a no-op: same Nix hash → same tarball → re-tag with no leftovers.
    #
    # Error handling: if podman is not on PATH (e.g. podman not enabled),
    # the script exits with a human-readable message rather than a silent no-op.
    #
    # Darwin note: this activation script is Linux-only. On macOS, podman runs
    # inside a Linux VM (`podman machine`). To load the image manually after
    # a rebuild, run:
    #   podman machine start && podman load < ${prismAgentImage}
    system.activationScripts.prismAgentContainerImage = ''
      if ! command -v podman >/dev/null 2>&1; then
        echo "ERROR: prism container-image activation: podman not found on PATH." >&2
        echo "       Ensure nx.programs.podman.enable = true and podman is installed." >&2
        exit 1
      fi
      echo "prism: loading prism-agent:latest into podman..." >&2
      podman image rm prism-agent:latest 2>/dev/null || true
      podman load < ${prismAgentImage}
      echo "prism: prism-agent:latest loaded successfully." >&2
    '';
  };
}
