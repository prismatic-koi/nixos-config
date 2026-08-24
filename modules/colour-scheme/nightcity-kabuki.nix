{
  config,
  lib,
  pkgs,
  ...
}:
let
  nightcityKabukiTheme = import ./palettes/nightcity-kabuki.nix { colourLib = import ./lib.nix; };
in
{
  theme = lib.mkIf (config.nx.desktop.theme == "nightcity-kabuki") nightcityKabukiTheme;
}
