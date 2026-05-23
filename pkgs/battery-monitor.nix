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
