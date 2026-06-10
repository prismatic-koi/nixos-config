{
  lib,
  buildGoModule,
  # When true, run the Go test suite inside the nix build sandbox.
  # Default is false so that local `nix build .#battery-monitor` and
  # `nh switch` are fast and do not re-run tests that the developer /
  # CI has already run via `go test ./...`. The
  # `nix-build-battery-monitor-checked` CI job (see
  # .github/workflows/pr-gate.yml) builds this package with
  # `runChecks = true` to preserve the homeless-shelter sandbox-
  # environment signal in the PR pipeline. Mirrors pkgs/prism.nix
  # exactly — see AGENTS.md for the rationale.
  runChecks ? false,
}:

buildGoModule {
  pname = "battery-monitor";
  version = "0.1.0";

  src = ../modules/services/battery-monitor/battery-monitor;

  # The Go module has exactly one entrypoint (main.go at the module
  # root). subPackages restricts compilation to that root so the
  # derivation produces only the battery-monitor binary.
  subPackages = [ "." ];

  doCheck = runChecks;

  # Override checkPhase so the test suite covers ./..., not just the
  # subPackages list. buildGoModule's default checkPhase iterates over
  # `getGoDirs test`, which honours `subPackages` when set — so with
  # `subPackages = [ "." ]` above (kept for buildPhase, which controls
  # the binary output), the default check would only test the root
  # package (`./.`), which has no test files. That silently masked the
  # homeless-shelter sandbox signal for every subpackage under
  # internal/... — the same regression pkgs/prism.nix had before #2168.
  # See issue #2173 and AGENTS.md § "the homeless-shelter failure class".
  #
  # The `GOFLAGS=${GOFLAGS//-trimpath/}` strip mirrors the default
  # checkPhase: buildGoModule adds -trimpath to GOFLAGS, which breaks
  # tests that reference assets via their source paths. Race detection
  # is intentionally not enabled here — the `go-tests-battery-monitor`
  # CI job already runs `go test ./... -race` on an Ubuntu runner; this
  # job's purpose is the $HOME=/homeless-shelter signal.
  #
  # The `-timeout 30m` flag is defensive: the suite finishes in well
  # under a second on a host, but the bwrap sandbox is dramatically
  # slower (see prism issue #2169 § Cluster 1), so we mirror the prism
  # budget rather than risk a flaky per-package 10m default.
  checkPhase = ''
    runHook preCheck
    export GOFLAGS=''${GOFLAGS//-trimpath/}
    go test -timeout 30m ./...
    runHook postCheck
  '';

  vendorHash = "sha256-WUTGAYigUjuZLHO1YpVhFSWpvULDZfGMfOXZQqVYAfs=";

  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Long-running user daemon that watches batteries and emits freedesktop notifications";
    mainProgram = "battery-monitor";
    license = lib.licenses.mit;
  };
}
