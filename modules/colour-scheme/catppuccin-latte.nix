{
  config,
  lib,
  pkgs,
  ...
}:
{
  theme = lib.mkIf (config.nx.desktop.theme == "catppuccin-latte") {
    name = "catppuccin-latte";
    opencodename = "catppuccin";
    type = "light";
    foreground = "#4c4f69";
    primary = "#40a02b";
    secondary = "#fe640b";
    red = "#d20f39";
    orange = "#fe640b";
    yellow = "#df8e1d";
    green = "#40a02b";
    aqua = "#209fb5";
    blue = "#1e66f5";
    purple = "#8839ef";
    grey0 = "#7c7f93";
    grey1 = "#6c6f85";
    grey2 = "#5c5f77";
    statusline1 = "#7287fd";
    statusline2 = "#df8e1d";
    statusline3 = "#e64553";
    bg_dim = "#eff1f5";
    bg0 = "#e6e9ef";
    bg1 = "#dce0e8";
    bg2 = "#ccd0da";
    bg3 = "#bcc0cc";
    bg4 = "#acb0be";
    bg5 = "#9ca0b0";
    bg_visual = "#04a5e5";
    bg_red = "#e64553";
    bg_green = "#40a02b";
    bg_blue = "#04a5e5";
    bg_yellow = "#df8e1d";
  };
}
