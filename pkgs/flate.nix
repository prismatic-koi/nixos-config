{
  lib,
  buildGoLatestModule,
  fetchFromGitHub,
  makeWrapper,
  coreutils,
}:

# flate v0.6.5 pin: this must move in lockstep with the home-ops CI pin.
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
  # a pure-Go tree walker when git isn't on PATH. No PATH injection is
  # needed here — makeWrapper is used below only to install the three
  # guards from issue #2983.

  nativeBuildInputs = [ makeWrapper ];

  # Three independent defences, wrapped at the derivation so every
  # consumer (the overlay, the flake package output,
  # modules/programs/kubetools.nix) inherits them. Each cures a
  # different failure and none substitutes for another — see issue
  # #2983.
  #
  # wrapProgram renames the built binary to $out/bin/.flate-wrapped and
  # installs the wrapper script at $out/bin/flate in its place. Guard 3
  # needs to invoke that renamed original directly (to run it under
  # `timeout` and inspect its exit code), so the guard script below
  # references it via the @hiddenFlate@ placeholder, substituted with
  # the real path before it's handed to wrapProgram.
  postFixup = ''
    guardScript=$(cat <<'GUARD'
    # Guard 1: linked-worktree guard (issue #2983).
    #
    # Every prism agent works in a linked git worktree, where .git is a
    # *file* (a gitdir pointer), not a directory. In that layout flate
    # cannot match the checkout to its GitRepository object: it falls
    # back to treating the source as remote, blocks on the absent
    # deploy key, and hangs instead of exiting. This cost three agents
    # 920s, 2800s, and 6200s during the home-ops migration.
    #
    # This check only walks up from the working directory to find the
    # nearest .git. It does not parse flate's own flags, so a path
    # passed via --path / --path-orig is out of scope.
    if [ -z "''${FLATE_ALLOW_WORKTREE:-}" ]; then
      _flate_guard_dir="$PWD"
      while :; do
        if [ -e "$_flate_guard_dir/.git" ]; then
          if [ -f "$_flate_guard_dir/.git" ]; then
            echo "flate: refusing to run from a linked git worktree ($_flate_guard_dir/.git is a file, not a directory)." >&2
            echo "flate cannot match a linked worktree checkout to its GitRepository object and will hang instead of exiting." >&2
            echo "Use the home-ops render script instead, or run flate against a non-worktree checkout. Set FLATE_ALLOW_WORKTREE=1 to bypass this guard." >&2
            exit 1
          fi
          break
        fi
        if [ "$_flate_guard_dir" = "/" ]; then
          break
        fi
        _flate_guard_dir="$(dirname "$_flate_guard_dir")"
      done
      unset _flate_guard_dir
    fi

    # Guard 3: overall invocation timeout, a backstop (issue #2983).
    #
    # Guards 1 and 2 each cure a hang already met. Neither bounds one
    # that hasn't happened yet. This bounds the whole invocation so an
    # unknown hang costs at most FLATE_TIMEOUT seconds instead of an
    # open-ended session. This wrapper is for agent local renders only
    # — home-ops CI pins its own flate and never sees this wrapper —
    # so the cost of a false kill is one retry with FLATE_TIMEOUT
    # raised.
    #
    # This runs after guard 1 above, so a linked-worktree refusal is
    # never delayed by entering the timed region.
    _flate_timeout="''${FLATE_TIMEOUT:-300}"
    if [ "$_flate_timeout" = "0" ]; then
      exec "@hiddenFlate@" "$@"
    fi
    # set -e is active in this wrapper (see the bash shebang makeWrapper
    # generates), so a bare non-zero-exit command would abort the script
    # before the exit-code check below ever ran. The `||` branch keeps
    # this a zero-status compound command so -e doesn't short-circuit it.
    _flate_status=0
    timeout "$_flate_timeout" "@hiddenFlate@" "$@" || _flate_status=$?
    if [ "$_flate_status" -eq 124 ]; then
      echo "flate wrapper: killed flate after ''${_flate_timeout}s (this is the wrapper's FLATE_TIMEOUT backstop, not a flate crash). Set FLATE_TIMEOUT=<seconds> to raise the limit, or FLATE_TIMEOUT=0 to disable it." >&2
    fi
    exit "$_flate_status"
    GUARD
    )
    guardScript="''${guardScript//@hiddenFlate@/$out/bin/.flate-wrapped}"
    wrapProgram $out/bin/flate \
      --set-default GIT_SSH_COMMAND "ssh -o BatchMode=yes -o ConnectTimeout=10" \
      --prefix PATH : ${lib.makeBinPath [ coreutils ]} \
      --run "$guardScript"
  '';

  meta = {
    description = "Local, offline validator and renderer for Flux GitOps repositories";
    homepage = "https://github.com/home-operations/flate";
    license = lib.licenses.agpl3Only;
    mainProgram = "flate";
  };
}
