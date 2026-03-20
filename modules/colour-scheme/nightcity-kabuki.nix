{
  config,
  lib,
  pkgs,
  ...
}:
{
  theme = lib.mkIf (config.nx.desktop.theme == "nightcity-kabuki") {
    name = "nightcity-kabuki";
    opencodename = "everforest";
    type = "dark";
    foreground = "#f9efc5";
    primary = "#ff9457";
    secondary = "#8db885";
    red = "#ff4b3b";
    orange = "#ff9457";
    yellow = "#ffbe32";
    green = "#9ea32a";
    aqua = "#8db885";
    blue = "#6e9685";
    purple = "#d9869e";
    grey0 = "#cfc6a6";
    grey1 = "#a39b85";
    grey2 = "#8d8675";
    statusline1 = "#89c5bf";
    statusline2 = "#fbf1c7";
    statusline3 = "#89c5bf";
    bg_dim = "#1b1b1b";
    bg0 = "#282828";
    bg1 = "#272623";
    bg2 = "#393633";
    bg3 = "#4a4542";
    bg4 = "#615b53";
    bg5 = "#777064";
    bg_visual = "#777064";
    bg_red = "#eb3040";
    bg_green = "#988921";
    bg_blue = "#5e8d6f";
    bg_yellow = "#eb8d27";
  };
}
