{
  lib,
  buildGoLatestModule,
  fetchFromGitHub,
}:

# flate v0.6.5 pin: this must move in lockstep with the home-ops CI pin
# once home-ops migrates its Flux rendering off flux-local onto flate.
# Do not put this derivation on any automated update surface (e.g.
# nix-update / renovate) — the version is intentionally hand-pinned to
# match home-ops.
let
  version = "0.6.5";
in
# go.mod declares `go 1.27.0`. The pinned nixpkgs default buildGoModule
# is pinned to go 1.26.7, which is too old to build this module;
# buildGoLatestModule (go 1.27.1) is the newest toolchain available in
# the pinned nixpkgs that satisfies the requirement.
buildGoLatestModule {
  pname = "flate";
  inherit version;

  src = fetchFromGitHub {
    owner = "home-operations";
    repo = "flate";
    rev = "v${version}";
    hash = "sha256-Z1bhf54xJSrCiLgRfzGuZ7ORzLgdFe5PfEVZzs8hkew=";
  };

  vendorHash = "sha256-6ZmGkdHW2/8wk/dKN9MkB+JF0/GFIw2TxZHeWShLsQ0=";

  subPackages = [ "cmd/flate" ];

  ldflags = [
    "-s"
    "-w"
  ];

  # flate embeds Helm and Kustomize rendering as Go libraries (helm.sh/
  # helm/v4, fluxcd/pkg/kustomize, etc.) rather than shelling out to the
  # `helm` / `kustomize` binaries — confirmed by grepping the v0.6.5
  # source tree for os/exec: the only use is an optional `git diff
  # --no-index` fast path in pkg/change/detect.go, which falls back to
  # a pure-Go tree walker when git isn't on PATH. No makeWrapper PATH
  # injection is needed (contrast pkgs/flux-local.nix, which does shell
  # out to helm/kustomize/flux).

  meta = {
    description = "Local, offline validator and renderer for Flux GitOps repositories";
    homepage = "https://github.com/home-operations/flate";
    license = lib.licenses.agpl3Only;
    mainProgram = "flate";
  };
}
