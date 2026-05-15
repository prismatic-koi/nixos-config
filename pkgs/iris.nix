{
  lib,
  buildGoModule,
  git,
  # When true, run the Go test suite inside the nix build sandbox.
  # Default is false so that local `nix build .#iris` and `nh switch`
  # are fast and do not re-run tests that the developer / CI has already
  # run via `go test ./...`. The `nix-build-iris-checked` CI job
  # (if added) would build this package with `runChecks = true` to
  # preserve the homeless-shelter sandbox-environment signal, mirroring
  # the pattern established by pkgs/prism.nix (see AGENTS.md and issue #1494).
  runChecks ? false,
}:

buildGoModule {
  pname = "iris";
  version = "0.1.0-d8";

  src = ../modules/programs/prism/prism;

  doCheck = runChecks;

  vendorHash = "sha256-tU+rnXKz3ALl7pJx7GYTo1hdr3CFMQS4Ih3UYLr4v54=";

  # Build only the iris entrypoint from the shared Go module.
  # subPackages restricts what buildGoModule compiles into the output,
  # so the derivation produces only the iris binary (not prism or any
  # other cmd/ entrypoints).
  subPackages = [ "cmd/iris" ];

  nativeCheckInputs = [ git ];

  meta = {
    description = "Iris — daemon-mode successor to prism (codename, D-3+)";
    mainProgram = "iris";
    license = lib.licenses.mit;
  };
}
