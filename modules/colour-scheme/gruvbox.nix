{
  config,
  lib,
  ...
}:
let
  gruvboxTheme = import ./palettes/gruvbox.nix { colourLib = import ./lib.nix; };
in
{
  theme = lib.mkMerge [
    (lib.mkIf (config.nx.desktop.theme == "gruvbox-light") gruvboxTheme.light)
    (lib.mkIf (config.nx.desktop.theme == "gruvbox-dark") gruvboxTheme.dark)
  ];
}
