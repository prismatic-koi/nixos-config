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

      # bitwarden-cli: pinned to nixpkgs-stable as the most-vetted source.
      # NOTE: pinning alone is no longer sufficient to prevent the `bw unlock
      # --raw` bogus-session regression (bitwarden/clients#20703, issue #1894).
      # nixpkgs-stable rolled forward to bitwarden-cli-2025.11.0 which still
      # carries the bug. The root cause is a server-pushed feature flag
      # (`unlock-via-sdk`) that triggers the broken code path regardless of
      # client version. The durable fix is the `unlock-via-sdk = false` patch
      # in modules/programs/bitwarden.nix (home.activation hook) and the
      # defensive patch in modules/programs/qutebrowser/userscripts/bitwarden-prefetch.
      # The pin is kept for institutional memory and because nixpkgs-stable
      # remains our most-vetted package source.
      # See: https://github.com/prismatic-koi/nixos-config/issues/1894
      bitwarden-cli = stablePkgs.bitwarden-cli;

      # bitwarden-desktop: interim master-pin. nixpkgs PR #545058
      # ("bitwarden-desktop: 2026.6.1 -> 2026.7.0") merged to nixpkgs
      # `master` on 2026-07-24, bumping the package off the EOL/insecure
      # electron version onto `electron_41` (upstream fix:
      # bitwarden/clients#20448, released as desktop-v2026.7.0).
      # That bump has NOT yet reached `nixos-unstable` (our `nixpkgs`
      # input), so we pin to `nixpkgs-master` here as an interim measure
      # rather than continuing to override the old package's EOL
      # electron with its `-bin` prebuilt variant.
      #
      # nixpkgs bump PR:      https://github.com/NixOS/nixpkgs/pull/545058
      # Tracking issue:       https://github.com/NixOS/nixpkgs/issues/526914
      #
      # The `electron_41` -> `electron_41-bin` override below exists only
      # to avoid compiling electron from source on uncached master revs
      # (a build-time concern, not a security requirement).
      #
      # REMOVAL CONDITION: delete this master-pin block entirely (reverting
      # to plain `nixos-unstable` bitwarden-desktop) once our `nixpkgs`
      # (`nixos-unstable`) input carries `bitwarden-desktop >= 2026.7.0` on
      # `electron >= 40`. At that point the `-bin` swap becomes a
      # judgement call, not a security requirement.
      bitwarden-desktop = masterPkgs.bitwarden-desktop.override {
        electron_41 = masterPkgs.electron_41-bin;
      };
      # direnv: disable the test phase on Darwin to work around a hang in
      # `direnv-test.zsh` introduced when libarchive was bumped 3.8.4 -> 3.8.6
      # on staging-25.11 (nixpkgs commit 32e655f). Direnv's source is
      # unchanged but transitive input changes force rebuilds, and the test
      # harness wedges on aarch64-darwin.
      # See: https://github.com/NixOS/nixpkgs/issues/507531
      # Remove this override once upstream lands a fix.
      direnv =
        if final.stdenv.isDarwin then
          prev.direnv.overrideAttrs (_: {
            doCheck = false;
          })
        else
          prev.direnv;

      # choose-gui: force the link step through `lld` on Darwin to work
      # around an `ld64` hardening regression that landed with the
      # nixpkgs staging-next merge on 2026-06-27 (flake bump
      # d407951 -> 0bb7ec5, PR #2380). The new hardening flag breaks
      # Objective-C / Cocoa-framework linking on aarch64-darwin; every
      # xcbuild-driven Cocoa build in the affected class fails at the
      # final `Ld` step. `choose-gui` builds via `xcbuild` and links
      # Cocoa, so it's in-class but did not get a per-package fix
      # upstream.
      #
      # nixpkgs shipped ~15 per-package workarounds with this exact
      # shape (add `llvmPackages.lld` to `nativeBuildInputs`, set
      # `NIX_CFLAGS_LINK = "-fuse-ld=lld"`), each carrying a
      # `# TODO: Clean up on \`staging\`.` comment. We mirror the shape
      # the maintainers used for `python3Packages.pyobjc-framework-Cocoa`
      # in https://github.com/NixOS/nixpkgs/pull/538151.
      #
      # REMOVAL CONDITION: delete this override once
      # https://github.com/NixOS/nixpkgs/pull/536365 ("ld64: disable
      # hardening again") lands on `staging` and a subsequent flake bump
      # picks up the reverted per-package workarounds. At that point the
      # underlying regression is gone and this block is dead weight.
      choose-gui =
        if final.stdenv.isDarwin then
          prev.choose-gui.overrideAttrs (old: {
            nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [ final.llvmPackages.lld ];
            env = (old.env or { }) // {
              NIX_CFLAGS_LINK = ((old.env.NIX_CFLAGS_LINK or "") + " -fuse-ld=lld");
            };
          })
        else
          prev.choose-gui;

      # kitty: force the link step through `lld` on Darwin to work around
      # the same `ld64` hardening regression that bit `choose-gui`
      # (see the block above). `kitty`'s `glfw-cocoa.so` link step
      # trips the same `Trace/BPT trap: 5` in the cctools ld wrapper:
      #
      #   clang ... -framework Cocoa -framework IOKit ...
      #     -o build/kitty/glfw-cocoa.so
      #   .../cctools-.../bin/ld: line 292: 24087 Trace/BPT trap: 5
      #   clang: error: linker command failed with exit code 133
      #
      # Failing m4mac CI run:
      #   https://github.com/prismatic-koi/nixos-config/actions/runs/29171272201
      #
      # Pattern origin: unlike `choose-gui`, this is a direct mirror of
      # the actual upstream fix — nixpkgs PR
      # https://github.com/NixOS/nixpkgs/pull/539908
      # ("kitty: work around ld64 hardening issue") landed on `master`
      # on 2026-07-09 as commit
      # d2a7c286bdb34760a36d51b5deb37b5e736e5c60. Our flake follows
      # `nixos-unstable`, and the channel has NOT yet advanced past
      # that commit — the bump we picked up in #2380 stopped at
      # `0bb7ec5`, which predates d2a7c286. So we mirror the fix
      # locally until the channel catches up.
      #
      # REMOVAL CONDITION: delete this override once EITHER
      #   (a) our `nixos-unstable` flake-follow advances past nixpkgs
      #       commit d2a7c286bdb34760a36d51b5deb37b5e736e5c60 (the
      #       kitty fix's merge commit) — at which point upstream
      #       carries the same override and ours is a duplicate; OR
      #   (b) https://github.com/NixOS/nixpkgs/pull/536365 ("ld64:
      #       disable hardening again") lands and the whole
      #       workaround train unwinds — at which point the
      #       underlying regression is gone and every per-package
      #       fix in this class (including upstream's own) becomes
      #       dead weight.
      # Whichever comes first.
      kitty =
        if final.stdenv.isDarwin then
          prev.kitty.overrideAttrs (old: {
            nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [ final.llvmPackages.lld ];
            env = (old.env or { }) // {
              NIX_CFLAGS_LINK = ((old.env.NIX_CFLAGS_LINK or "") + " -fuse-ld=lld");
            };
          })
        else
          prev.kitty;

      # openscad-unstable: pinned to nixpkgs-stable as the unstable build
      # in nixpkgs-unstable is broken due to an LTO / ld.lld linker error
      # (`.debug_gdb_scripts: string is not null terminated`) on source builds.
      # This blocks the nightly flake updates. The stable version builds
      # successfully and does not require frequent updates.
      #
      # See: https://github.com/NixOS/nixpkgs/issues/543373
      #
      # REMOVAL CONDITION: delete this pin once the upstream openscad-unstable
      # build failure is fixed and a subsequent flake bump picks up the working
      # derivation from nixpkgs-unstable.
      openscad-unstable = stablePkgs.openscad-unstable;

      # pi-coding-agent: pinned to 0.82.1 via overrideAttrs. nixpkgs PR #545612
      # ("pi-coding-agent: 0.81.1 -> 0.82.1") is still OPEN against nixpkgs
      # `master` as of writing, so the bump has not reached any of our
      # nixpkgs inputs (`nixpkgs`, `nixpkgs-master`, `nixpkgs-stable`). Our
      # `nixpkgs` input is pinned well behind even the PR's base revision
      # (0.80.10, predating the `modelData`/`preConfigure` model-catalog
      # restore step), so the override below also carries that structure
      # forward rather than relying on it already being present upstream.
      #
      # nixpkgs bump PR: https://github.com/NixOS/nixpkgs/pull/545612
      #
      # REMOVAL CONDITION: delete this override once our `nixpkgs`
      # (`nixos-unstable`) input carries `pi-coding-agent >= 0.82.1`.
      pi-coding-agent = prev.pi-coding-agent.overrideAttrs (
        finalAttrs: _prevAttrs: {
          version = "0.82.1";

          src = final.fetchFromGitHub {
            owner = "earendil-works";
            repo = "pi";
            tag = "v${finalAttrs.version}";
            hash = "sha256-LESpgd/KUoNqdBfnd1oyMN8coKm0Odbo9GYkUDry8Zk=";
          };

          npmDepsHash = "sha256-5pHRwxpKg95/phOcYHeWdvPJNtSOhiw7PRoVxsuh0RM=";

          # `npmDeps` is baked in by `buildNpmPackage` from the *original*
          # `src`/`npmDepsHash` at construction time; overriding those two
          # attrs alone does not cause it to be recomputed, so it must be
          # overridden explicitly here too or the build fetches deps for
          # the old (0.80.10) `package-lock.json`.
          npmDeps = final.fetchNpmDeps {
            name = "pi-coding-agent-${finalAttrs.version}-npm-deps";
            src = finalAttrs.src;
            hash = finalAttrs.npmDepsHash;
          };

          # The provider model catalog (packages/ai/src/providers/data/) is
          # generated by a network fetch and is gitignored upstream, so it's
          # absent from the source tarball. Restore it from the matching
          # published @earendil-works/pi-ai npm package.
          modelData = final.fetchurl {
            url = "https://registry.npmjs.org/@earendil-works/pi-ai/-/pi-ai-${finalAttrs.version}.tgz";
            hash = "sha256-L535UigItiHNNEmHZTfwPYqN+LjX7C1bGMapEKqFtJA=";
          };

          preConfigure = ''
            mkdir -p packages/ai/src/providers/data
            tar --extract --gzip --file=${finalAttrs.modelData} \
              --directory=packages/ai/src/providers/data \
              --strip-components=4 \
              package/dist/providers/data
          '';
        }
      );

      # qutebrowser: widen the built-in AMD+Wayland GBM workaround guard in
      # `misc/backendproblem.py::_fix_wayland_amd_gbm` from exact QtWebEngine
      # 6.11.0 to all 6.11.x. Upstream self-deactivated the workaround on
      # 6.11.1 assuming Qt fixed the underlying AMD GBM bug, but on our
      # amdgpu+Hyprland machine (navi) the bug persists on 6.11.1: page
      # elements render transparent/broken until `QTWEBENGINE_FORCE_USE_GBM=0`
      # is set. Manually replicating that env var restores rendering, so we
      # patch the version guard to `strip_patch() != VersionNumber(6, 11)` so
      # every 6.11.x re-enters the known-good 6.11.0 code path.
      #
      # Upstream references:
      #   https://github.com/qutebrowser/qutebrowser/issues/8841
      #   https://github.com/NixOS/nixpkgs/issues/531703
      #
      # REMOVAL CONDITION: delete this override (and its patch file) once
      # either
      #   (a) upstream qutebrowser widens the guard itself (e.g. matching
      #       all 6.11.x, or dropping the version check entirely), or
      #   (b) Qt lands a real fix for the AMD+Wayland GBM path and the
      #       workaround is no longer needed on our hardware — in which
      #       case the whole `_fix_wayland_amd_gbm` codepath becomes
      #       obsolete upstream and this patch with it.
      qutebrowser = addPatches prev.qutebrowser [
        ./qutebrowser-widen-amd-gbm-workaround.patch
      ];

      # packages not yet in nixpkgs; use local definitions
      # playwright-cli is cross-platform: on Linux it uses pkgs.chromium
      # (the user's daily-driver browser, closure already paid for); on
      # Darwin it uses pkgs.playwright-driver.browsers-chromium (the
      # playwright-pinned "Google Chrome for Testing" build, identity-
      # isolated from the user's Chrome.app).
      playwright-cli = final.callPackage ../pkgs/playwright-cli.nix { };

      prism = final.callPackage ../pkgs/prism.nix { };

      # battery-monitor: Linux-only Go daemon (UPower + sysfs +
      # session-bus notifications). See pkgs/battery-monitor.nix and
      # modules/services/battery-monitor/DESIGN.md.
      battery-monitor =
        if final.stdenv.isLinux then final.callPackage ../pkgs/battery-monitor.nix { } else null;

      _macronTypePkg =
        if final.stdenv.isDarwin then final.callPackage ../pkgs/macron-type.nix { } else null;
      macron-type = if final.stdenv.isDarwin then final._macronTypePkg.server else null;
      macron-send = if final.stdenv.isDarwin then final._macronTypePkg.client else null;

    };
}
