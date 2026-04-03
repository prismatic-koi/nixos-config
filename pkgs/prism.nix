{
  lib,
  buildGoModule,
  git,
  tmux,
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

  nativeCheckInputs = [ git ];

  preCheck = ''
    export GIT_CONFIG_NOSYSTEM=1
    export HOME=$(mktemp -d)
    git config --global user.email "test@test.com"
    git config --global user.name "Test"
  '';

  meta = {
    description = "Prism — tmux-based AI development environment TUI";
    mainProgram = "prism";
    license = lib.licenses.mit;
  };
}
