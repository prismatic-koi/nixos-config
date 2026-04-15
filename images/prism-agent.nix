# Standalone prism-agent container image derivation.
#
# This file is a pure function that builds the prism-agent OCI image for a
# given pkgs (which determines the target architecture and package versions).
#
# Arguments:
#   pkgs               — nixpkgs package set for the target architecture
#                        (e.g. pkgs for x86_64-linux or aarch64-linux)
#   lib                — nixpkgs lib (passed separately so callers can use
#                        their own lib without pulling in pkgs.lib)
#   cachixSubstituters — list of extra substituter URLs to bake into the image's
#                        nix.conf (e.g. ["https://lucidph3nx-nixos-config.cachix.org"])
#   trustedPublicKeys  — list of trusted public keys for the above substituters
#
# This function is called by:
#   - flake.nix (exposes packages.x86_64-linux.prismAgentImage and
#     packages.aarch64-linux.prismAgentImage for CI to build and push to GHCR)
{
  pkgs,
  lib,
  cachixSubstituters ? [ ],
  trustedPublicKeys ? [ ],
}:

let
  # Ubuntu 24.04 LTS base image pulled at evaluation time.
  # Architecture-aware: selects the correct digest for the target platform so
  # that both the base layer and the Nix-built contents are for the same arch.
  #
  # To refresh these digests when the upstream image changes:
  #   amd64: nix run nixpkgs#nix-prefetch-docker -- ubuntu 24.04 --os linux --arch amd64
  #   arm64: nix run nixpkgs#nix-prefetch-docker -- ubuntu 24.04 --os linux --arch arm64
  ubuntuBase =
    if pkgs.stdenv.hostPlatform.system == "aarch64-linux" then
      pkgs.dockerTools.pullImage {
        imageName = "ubuntu";
        imageDigest = "sha256:11c7dd0cbd7effee6cd4f11811caffb5fdf682f1667c7f152c5cee7d32cc337c";
        hash = "sha256-IbZLKzBX0herRzTZqhr/ORNwiPnrnmlQq1i2TxbEt4g=";
        finalImageName = "ubuntu";
        finalImageTag = "24.04";
        os = "linux";
        arch = "arm64";
      }
    else
      pkgs.dockerTools.pullImage {
        imageName = "ubuntu";
        imageDigest = "sha256:186072bba1b2f436cbb91ef2567abca677337cfc786c86e107d25b7072feef0c";
        hash = "sha256-8DXLXnojKTuXpneMIHCEvcqJRHyA4dAKmMYE36QGMMU=";
        finalImageName = "ubuntu";
        finalImageTag = "24.04";
        os = "linux";
        arch = "amd64";
      };

  # Build nix.conf content from the provided substituter config so it stays in
  # sync with the caller's nix settings.
  nixConfLines = [
    "experimental-features = nix-command flakes"
  ]
  ++ lib.optionals (cachixSubstituters != [ ]) [
    "extra-substituters = ${lib.concatStringsSep " " cachixSubstituters}"
    "extra-trusted-public-keys = ${lib.concatStringsSep " " trustedPublicKeys}"
  ];
  nixConfFile = pkgs.writeText "container-nix.conf" (lib.concatStringsSep "\n" nixConfLines + "\n");

  # nixos-rebuild guard — prevents agents from accidentally trying to apply
  # NixOS configuration inside a container (which would fail without systemd).
  nixosRebuildGuard = pkgs.writeShellScript "nixos-rebuild" ''
    echo "error: nixos-rebuild is not available inside agent containers" >&2
    echo "Use 'nix build' to validate the flake instead." >&2
    exit 1
  '';
in
# The prism agent container image.
# Built with buildLayeredImage so each Nix package gets its own layer —
# podman can cache unchanged layers and only reload what changed.
#
# `contents` accepts a list of derivations to copy to the image root.
# Wrapping packages in buildEnv merges them into a single /bin, /lib,
# etc. symlink farm so binaries land on PATH. Passing raw package
# derivations directly would place them at their Nix store paths only,
# leaving them unreachable via PATH.
#
# The packages are split into two buildEnv groups.  buildLayeredImage merges
# all contents entries via symlinkJoin into one customisation layer, but the
# per-package layers come from the Nix store closure graph — each store path
# gets its own layer.  By isolating opencode/claude-code/prism into their own
# buildEnv, the ai-tooling store path (and its closure) changes independently
# from the stable-infra store path.  When only the AI tools bump, only those
# layers in the closure are invalidated; the stable-infra closure layers
# remain cache-hits in podman.
pkgs.dockerTools.buildLayeredImage {
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

    # Stable infrastructure tools — rarely change, so this layer is almost
    # always cache-hit when only AI tooling is updated.
    (pkgs.buildEnv {
      name = "prism-agent-stable-infra";
      paths = with pkgs; [
        # Shell and core utilities — pin the Nix versions so PATH is consistent
        # regardless of what Ubuntu provides
        bash
        coreutils

        # Development tools
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

        # Cloud and infrastructure
        kubectl
        kubernetes-helm
        awscli2
        opentofu
        fluxcd

        # Secrets tooling — agents may read or edit sops-encrypted files
        sops
        age
        openssl

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

    # AI tooling — updated frequently; isolated so the stable-infra layer
    # above is unaffected when opencode, claude-code, or prism bump versions.
    # Note: /etc is intentionally omitted from pathsToLink — these packages
    # don't install to /etc, and including it would risk a symlinkJoin
    # collision with cacert's /etc/ssl/certs/ from the stable-infra group.
    (pkgs.buildEnv {
      name = "prism-agent-ai-tooling";
      paths = with pkgs; [
        opencode
        claude-code
        # Prism CLI — agents need prism spawn, prompt, checkin, etc.
        prism
      ];
      pathsToLink = [
        "/bin"
        "/lib"
        "/share"
      ];
    })
  ];

  extraCommands = ''
    # Nix configuration — experimental features plus substituters/keys
    # sourced from the caller's nix settings so they stay in sync with the host.
    mkdir -p etc/nix
    cp ${nixConfFile} etc/nix/nix.conf

    # nixos-rebuild guard
    cp ${nixosRebuildGuard} bin/nixos-rebuild
    chmod +x bin/nixos-rebuild

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
}
