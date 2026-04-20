{
  outputs,
  inputs,
}:
let
  addPatches =
    pkg: patches:
    pkg.overrideAttrs (oldAttrs: {
      patches = (oldAttrs.patches or [ ]) ++ patches;
    });
in
rec {
  modifications =
    final: prev:
    let
      masterPkgs = import inputs.nixpkgs-master {
        system = final.stdenv.hostPlatform.system;
        config.allowUnfree = true;
      };
      stablePkgs = import inputs.nixpkgs-stable {
        system = final.stdenv.hostPlatform.system;
        config.allowUnfree = true;
      };
    in
    {
      # packages where we use master by default for bleeding edge

      claude-code = masterPkgs.claude-code;
      discord = masterPkgs.discord;
      # Pinned to opencode 1.14.18 via overrideAttrs on nixpkgs' opencode.
      # History: 1.4.11 regressed the --prompt TUI flag (pre-fills instead of
      # auto-submitting), which broke prism's headless workflows, so we
      # previously pinned 1.4.6 via a dedicated `nixpkgs-opencode` flake input
      # (see #901). 1.14.18 is verified to no longer carry that regression, so
      # we can pin by overrideAttrs instead of dragging a whole nixpkgs
      # revision along for one package (see #910).
      #
      # The 1.14.x series changed the build contract vs. the 1.4.x that is
      # still in `prev.opencode`: the bundled CLI now dynamically imports
      # `prettier` (a root-workspace devDep) inside `generate.ts`, so the
      # root workspace must be included in the `bun install` filter set;
      # `packages/shared` is a new standalone workspace; and the schema
      # script emits a single `schema.json` instead of the old
      # `config.json` + `tui.json` pair. So we have to override the build /
      # install phases in addition to the version + hashes.
      #
      # To bump this pin:
      #   1. Update `version` below.
      #   2. Recompute `src.hash`:
      #        nix-prefetch-url --unpack \
      #          https://github.com/anomalyco/opencode/archive/refs/tags/v<version>.tar.gz
      #        nix hash convert --hash-algo sha256 --to sri <hash-from-above>
      #   3. Recompute `node_modules.outputHash`: set it to `lib.fakeHash`,
      #      attempt a build, and substitute the correct hash from the error.
      opencode = prev.opencode.overrideAttrs (old: rec {
        version = "1.14.18";
        src = old.src.override {
          tag = "v${version}";
          hash = "sha256-wEjksPEPzEe2BCySqjorMXrbnBWNCp+YAaCiZWV2ZIc=";
        };

        node_modules = old.node_modules.overrideAttrs (_: {
          inherit src;
          # 1.14.x: include the root workspace (`--filter './'`) so that
          # `prettier` — a root devDep that `generate.ts` dynamically
          # imports — ends up in node_modules, and add `packages/shared`
          # which is a new standalone workspace in 1.14.x.
          buildPhase = ''
            runHook preBuild

            bun install \
              --cpu="*" \
              --frozen-lockfile \
              --filter './' \
              --filter ./packages/app \
              --filter ./packages/desktop \
              --filter ./packages/opencode \
              --filter ./packages/shared \
              --ignore-scripts \
              --no-progress \
              --os="*"

            bun --bun ./nix/scripts/canonicalize-node-modules.ts
            bun --bun ./nix/scripts/normalize-bun-binaries.ts

            runHook postBuild
          '';
          outputHash = "sha256-KZ/tfg84DNa56VglMWmExE3R219BXwxSmhKnLiQWOV4=";
        });

        # 1.14.x: schema script now emits a single `schema.json` rather than
        # `config.json` + `tui.json`, and the install step needs to follow.
        buildPhase = ''
          runHook preBuild

          cd ./packages/opencode
          bun --bun ./script/build.ts --single --skip-install
          bun --bun ./script/schema.ts schema.json

          runHook postBuild
        '';

        installPhase = ''
          runHook preInstall

          install -Dm755 dist/opencode-*/bin/opencode $out/bin/opencode
          install -Dm644 schema.json $out/share/opencode/schema.json

          wrapProgram $out/bin/opencode \
            --prefix PATH : ${
              final.lib.makeBinPath (
                [ final.ripgrep ] ++ final.lib.optional final.stdenv.hostPlatform.isDarwin final.sysctl
              )
            }

          runHook postInstall
        '';

        passthru = (old.passthru or { }) // {
          jsonschema = "${placeholder "out"}/share/opencode/schema.json";
        };
      });
      pi-coding-agent = masterPkgs.pi-coding-agent;

      # packages not yet in nixpkgs; use local definitions
      # playwright-cli depends on chromium which is Linux-only
      playwright-cli =
        if final.stdenv.isLinux then final.callPackage ../pkgs/playwright-cli.nix { } else null;

      prism = final.callPackage ../pkgs/prism.nix { };

      _macronTypePkg =
        if final.stdenv.isDarwin then final.callPackage ../pkgs/macron-type.nix { } else null;
      macron-type = if final.stdenv.isDarwin then final._macronTypePkg.server else null;
      macron-send = if final.stdenv.isDarwin then final._macronTypePkg.client else null;

    };
}
