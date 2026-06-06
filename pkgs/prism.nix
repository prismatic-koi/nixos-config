{
  lib,
  buildGoModule,
  git,
  tmux,
  # When true, run the Go test suite inside the nix build sandbox.
  # Default is false so that local `nix build .#prism` and `nh switch`
  # are fast and do not re-run tests that the developer / CI has already
  # run via `go test ./...`. The `nix-build-prism-checked` CI job
  # (see .github/workflows/pr-gate.yml) builds this package with
  # `runChecks = true` to preserve the homeless-shelter sandbox-environment
  # signal in the PR pipeline (see AGENTS.md and issue #1494).
  runChecks ? false,
}:

buildGoModule {
  pname = "prism";
  version = "0.1.0";

  src = ../modules/programs/prism/prism;

  # Build only the prism entrypoint from the shared Go module.
  # subPackages restricts what buildGoModule compiles into the output,
  # so the derivation produces only the prism binary (not any other
  # cmd/ entrypoints).
  subPackages = [ "." ];

  doCheck = runChecks;

  vendorHash = "sha256-QmGhhx3JmxoNj8cgTaOIS4nffHx/vrB/fgXFjmle1gA=";

  # reviewGoSHA is the SHA-256 of internal/review/review.go, computed at
  # build time via builtins.hashFile so it is content-addressed and changes
  # whenever the file changes (C.4.PT, issue #1148). The first 12 characters
  # mirror the git short-SHA convention: short enough to be human-readable,
  # long enough to be unambiguous in practice.
  ldflags =
    let
      reviewGoHash = builtins.hashFile "sha256" ../modules/programs/prism/prism/internal/review/review.go;
    in
    [
      "-s"
      "-w"
      "-X github.com/prismatic-koi/prism/internal/tmux.TmuxBin=${tmux}/bin/tmux"
      "-X github.com/prismatic-koi/prism/internal/review.reviewGoSHA=${
        builtins.substring 0 12 reviewGoHash
      }"
    ];

  # tmux is NOT in nativeCheckInputs.
  #
  # The cmd/ and internal/tmux/ integration tests use script(1) to fake a
  # PTY for tmux client attachment (attachClientToSession in dashboard_test.go
  # and harness_test.go). In the nix build sandbox (bwrap with --dev /dev),
  # script(1) can run but the PTY slave cannot become a controlling terminal
  # for tmux's client process, so the client never appears in list-clients
  # regardless of timing. Multi-client PTY tests are also broken in the bwrap
  # sandbox (a second concurrent PTY client causes both to exit immediately
  # due to devpts namespace constraints).
  #
  # Tests that call attachClientToSession call skipIfSandboxPTY(t) at the
  # top, which explicitly skips with a descriptive message when running in a
  # bwrap sandbox ($NIX_BUILD_TOP set, or /proc/1/comm == "bwrap"). In cmd/,
  # all such tests — including single-client ones — are guarded because the
  # nix sandbox breaks single-client PTY attachment too. In internal/tmux/,
  # only multi-client tests are guarded: single-client tests work in bwrap
  # (one client doesn't trigger the devpts issue) and skip via the tmux
  # PATH check in the nix sandbox (tmux not being in nativeCheckInputs).
  # This is NOT a silent skip — it prints the reason. The anti-pattern was
  # the SILENT skip caused by tmux not being in PATH; that is now replaced
  # with explicit skips that say exactly why the test cannot run.
  #
  # To run the PTY integration tests: `go test ./cmd/...` or
  # `go test ./internal/tmux/...` from a host shell (not inside a prism spawn
  # or nix build), where a real controlling terminal is available.
  nativeCheckInputs = [ git ];

  meta = {
    description = "Prism — tmux-based AI development environment TUI";
    mainProgram = "prism";
    license = lib.licenses.mit;
  };
}
