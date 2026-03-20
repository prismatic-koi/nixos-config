{
  config,
  lib,
  pkgs,
  ...
}:
{
  theme = lib.mkIf (config.nx.desktop.theme == "everforest") {
    name = "everforest";
    opencodename = "everforest";
    type = "dark";
    foreground = "#d3c6aa";
    primary = "#a7c080";
    secondary = "#7fbbb3";
    red = "#e67e80";
    orange = "#e69875";
    yellow = "#dbbc7f";
    green = "#a7c080";
    aqua = "#83c092";
    blue = "#7fbbb3";
    purple = "#d699b6";
    grey0 = "#7a8478";
    grey1 = "#859289";
    grey2 = "#9da9a0";
    statusline1 = "#a7c080";
    statusline2 = "#d3c6aa";
    statusline3 = "#e67e80";
    bg_dim = "#232a2e";
    bg0 = "#2d353b";
    bg1 = "#343f44";
    bg2 = "#3d484d";
    bg3 = "#475258";
    bg4 = "#4f585e";
    bg5 = "#56635f";
    bg_visual = "#543a48";
    bg_red = "#514045";
    bg_green = "#425047";
    bg_blue = "#3a515d";
    bg_yellow = "#4d4c43";
  };
}
