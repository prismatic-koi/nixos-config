{
  config,
  lib,
  pkgs,
  ...
}:
{
  imports = [ ./schema.nix ];

  theme = lib.mkIf (config.nx.desktop.theme == "onedark") {
    name = "onedark";
    opencodename = "one-dark";
    type = "dark";
    foreground = "#abb2bf";
    primary = "#98c379";
    secondary = "#61afef";
    red = "#e86671";
    orange = "#d19a66";
    yellow = "#e5c07b";
    green = "#98c379";
    aqua = "#56b6c2";
    blue = "#61afef";
    purple = "#c678dd";
    grey0 = "#535965";
    grey1 = "#5c6370";
    grey2 = "#848b98";
    statusline1 = "#98c379";
    statusline2 = "#e5c07b";
    statusline3 = "#e86671";
    bg_dim = "#21252b";
    bg0 = "#282c34";
    bg1 = "#31353f";
    bg2 = "#393f4a";
    bg3 = "#3b3f4c";
    bg4 = "#3b3f4c";
    bg5 = "#3b3f4c";
    bg_visual = "#382b2c";
    bg_red = "#8b3434";
    bg_green = "#8cc265";
    bg_blue = "#2c5372";
    bg_yellow = "#93691d";
  };
}
