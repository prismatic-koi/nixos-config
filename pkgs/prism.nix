{
  lib,
  buildGoModule,
  # Theme colours injected so the binary matches the user's active theme.
  # Each is a #rrggbb hex string.
  colorPrimary ? "#d4be98",
  colorSecondary ? "#a89984",
  colorPurple ? "#d3869b",
  colorYellow ? "#d8a657",
  colorGreen ? "#a9b665",
  colorBlue ? "#7daea3",
  colorRed ? "#ea6962",
  colorBgVisual ? "#543a48",
  colorForeground ? "#d3c6aa",
  # Project/worktree config (colon-separated strings).
  worktreeExclude ? "obsidian",
  projectLocations ? "~/code",
  projectSpecific ? "~/documents/obsidian",
}:

buildGoModule {
  pname = "prism";
  version = "0.1.0";

  src = ../modules/programs/prism/prism;

  # vendor/ directory is committed, so buildGoModule uses it directly
  vendorHash = null;

  env.CGO_ENABLED = "0";

  ldflags = [
    "-s"
    "-w"
    "-X github.com/prismatic-koi/prism/cmd.ColorPrimary=${colorPrimary}"
    "-X github.com/prismatic-koi/prism/cmd.ColorSecondary=${colorSecondary}"
    "-X github.com/prismatic-koi/prism/cmd.ColorPurple=${colorPurple}"
    "-X github.com/prismatic-koi/prism/cmd.ColorYellow=${colorYellow}"
    "-X github.com/prismatic-koi/prism/cmd.ColorGreen=${colorGreen}"
    "-X github.com/prismatic-koi/prism/cmd.ColorBlue=${colorBlue}"
    "-X github.com/prismatic-koi/prism/cmd.ColorRed=${colorRed}"
    "-X github.com/prismatic-koi/prism/cmd.ColorBgVisual=${colorBgVisual}"
    "-X github.com/prismatic-koi/prism/cmd.ColorForeground=${colorForeground}"
    "-X github.com/prismatic-koi/prism/cmd.SwitchWorktreeExclude=${worktreeExclude}"
    "-X github.com/prismatic-koi/prism/cmd.SwitchProjectLocations=${projectLocations}"
    "-X github.com/prismatic-koi/prism/cmd.SwitchProjectSpecific=${projectSpecific}"
  ];

  meta = {
    description = "Prism — tmux-based AI development environment TUI";
    mainProgram = "prism";
    license = lib.licenses.mit;
  };
}
