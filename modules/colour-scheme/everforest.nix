{
  config,
  lib,
  pkgs,
  ...
}:
let
  everforestTheme = import ./palettes/everforest.nix { colourLib = import ./lib.nix; };
in
{
  theme = lib.mkIf (config.nx.desktop.theme == "everforest") everforestTheme;
}
