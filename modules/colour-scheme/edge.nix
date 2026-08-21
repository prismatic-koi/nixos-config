{
  config,
  lib,
  pkgs,
  ...
}:
let
  edgeThemev2 = import ./themev2/edge.nix { colourLib = import ./lib.nix; };
in
{
  # Parallel themev2 schema (migration increment #1). Additive: no consumer
  # reads themev2 yet. See ./themev2/edge.nix.
  themev2 = lib.mkIf (config.nx.desktop.theme == "edge") edgeThemev2;

  theme = lib.mkIf (config.nx.desktop.theme == "edge") {
    name = "edge";
    type = "dark";
    foreground = "#c5cdd9";
    primary = "#a0c980";
    secondary = "#6cb6eb";
    red = "#ec7279";
    orange = "#e59676";
    yellow = "#deb974";
    green = "#a0c980";
    aqua = "#5dbbc1";
    blue = "#6cb6eb";
    purple = "#d38aea";
    grey0 = "#535c6a";
    grey1 = "#758094";
    grey2 = "#828a98";
    statusline1 = "#a0c980";
    statusline2 = "#c5cdd9";
    statusline3 = "#ec7279";
    bg_dim = "#24262a";
    bg0 = "#2c2e34";
    bg1 = "#33353f";
    bg2 = "#363944";
    bg3 = "#3b3e48";
    bg4 = "#414550";
    bg5 = "#414550";
    bg_visual = "#493c53";
    bg_red = "#55393d";
    bg_green = "#394634";
    bg_blue = "#354157";
    bg_yellow = "#4e432f";
  };
}
