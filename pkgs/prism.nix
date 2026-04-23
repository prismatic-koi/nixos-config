{
  lib,
  buildGoModule,
  git,
  tmux,
  util-linux,
}:

buildGoModule {
  pname = "prism";
  version = "0.1.0";

  src = ../modules/programs/prism/prism;

  vendorHash = "sha256-tU+rnXKz3ALl7pJx7GYTo1hdr3CFMQS4Ih3UYLr4v54=";

  ldflags = [
    "-s"
    "-w"
    "-X github.com/prismatic-koi/prism/internal/tmux.TmuxBin=${tmux}/bin/tmux"
  ];

  # tmux and util-linux (for script(1)) are needed for the integration tests
  # in cmd/ that start isolated tmux servers and attach clients via script.
  # Without tmux in PATH, those tests skip silently via t.Skip() in
  # newCmdTestServer — but they should run in CI.
  #
  # util-linux provides script(1), used by attachClientToSession to fake a PTY
  # so tmux will accept the attach. It is not available in the sandbox by default.
  #
  # Note: multi-client PTY tests (tests that attach two simultaneous script-based
  # clients) are explicitly skipped when running inside a bwrap sandbox (the nix
  # check phase uses bwrap), because bwrap's devpts namespace prevents a second
  # concurrent script-attached tmux client from appearing in list-clients. Those
  # tests call skipIfBwrapMultiClient(t) with a descriptive message; they are
  # not silent. Single-client integration tests run fine under bwrap.
  nativeCheckInputs = [
    git
    tmux
    util-linux
  ];

  meta = {
    description = "Prism — tmux-based AI development environment TUI";
    mainProgram = "prism";
    license = lib.licenses.mit;
  };
}
