{
  config,
  lib,
  pkgs,
  ...
}:
let
  githubLightTheme = import ./palettes/github-light.nix { colourLib = import ./lib.nix; };
in
{
  theme = lib.mkIf (config.nx.desktop.theme == "github-light") githubLightTheme;
}
