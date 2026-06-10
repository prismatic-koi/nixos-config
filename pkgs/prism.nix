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

  # Override checkPhase so the test suite covers ./..., not just the
  # subPackages list. buildGoModule's default checkPhase iterates over
  # `getGoDirs test`, which honours `subPackages` when set — so with
  # `subPackages = [ "." ]` above (kept for buildPhase, which controls
  # the binary output), the default check would only test the root
  # package (`./.`), which has no test files. That silently masked the
  # homeless-shelter sandbox signal for every subpackage under
  # internal/... and cmd/... See issue tracking this regression and
  # AGENTS.md § "the homeless-shelter failure class".
  #
  # The `GOFLAGS=${GOFLAGS//-trimpath/}` strip mirrors the default
  # checkPhase: buildGoModule adds -trimpath to GOFLAGS, which breaks
  # tests that reference assets via their source paths. Race detection
  # is intentionally not enabled here — the `go-tests` CI job already
  # runs `go test ./... -race` on an Ubuntu runner; this job's purpose
  # is the $HOME=/homeless-shelter signal, not race coverage.
  #
  # The `-timeout 30m` flag overrides Go's default 10m per-package budget.
  # This is a deliberate, permanent calibration for the nix-sandbox
  # environment; it is not an interim workaround.
  #
  # Empirical baseline:
  #   - `internal/db` (~270 tests, each calling db.Open which runs all
  #     schema migrations): ~3.5s on a host shell, ~600s inside the nix
  #     build sandbox — a ~170× slowdown.
  #   - `internal/sidecar` (~150 tests, each constructing a Sidecar with
  #     fresh db.Open + Unix-socket listeners + many goroutines): finishes
  #     in tens of seconds on a host, brushes against the 10m default in
  #     the sandbox.
  #
  # Root cause: the slowdown is dominated by per-syscall overhead, not
  # fsync — a host-side microbench shows `synchronous=OFF` and
  # `temp_store=MEMORY` produce no measurable speedup (db.Open averages
  # ~6ms baseline and ~5.7ms with both pragmas). The most likely culprit
  # is bwrap's per-syscall cost (user-namespace remapping + seccomp filter
  # evaluation) multiplied by the many small syscalls modernc.org/sqlite
  # makes per Open + per query. Mitigating it cleanly would require
  # changing how the production driver opens databases (e.g. an in-memory
  # mode for tests, or batching the migration run), which is out of scope
  # for the gate — the gate's job is to surface $HOME-touching failures,
  # not to optimise SQLite-in-bwrap throughput.
  #
  # 30m gives ~3× headroom over the worst observed package runtime (600s),
  # which absorbs CI runner load variance without masking a genuine
  # infinite-loop regression. Raising it further would weaken the
  # "a stuck test eventually fails" signal without practical benefit.
  checkPhase = ''
    runHook preCheck
    export GOFLAGS=''${GOFLAGS//-trimpath/}
    go test -timeout 30m ./...
    runHook postCheck
  '';

  vendorHash = "sha256-EzwMXDWKZZ4rVmbZVF5fpw7kZbeHphfxkolNeaXKQo8=";

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
