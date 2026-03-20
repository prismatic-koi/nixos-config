{
  config,
  lib,
  pkgs,
  ...
}:
{
  theme = lib.mkIf (config.nx.desktop.theme == "github-light") {
    name = "github-light";
    opencodename = "github";
    type = "light";
    foreground = "#24292f";
    primary = "#0366d6";
    secondary = "#116329";
    red = "#cf222e";
    orange = "#fb8f44";
    yellow = "#d4a72c";
    green = "#116329";
    aqua = "#1b7c83";
    blue = "#0969da";
    purple = "#8250df";
    grey0 = "#6e7781";
    grey1 = "#57606a";
    grey2 = "#424a53";
    statusline1 = "#0969da";
    statusline2 = "#6e7781";
    statusline3 = "#cf222e";
    bg_dim = "#ffffff";
    bg0 = "#ffffff";
    bg1 = "#f6f8fa";
    bg2 = "#eaeef2";
    bg3 = "#d0d7de";
    bg4 = "#afb8c1";
    bg5 = "#afb8c1";
    bg_visual = "#ddf4ff";
    bg_red = "#ffebe9";
    bg_green = "#dafbe1";
    bg_blue = "#ddf4ff";
    bg_yellow = "#fff8c5";
  };
}
