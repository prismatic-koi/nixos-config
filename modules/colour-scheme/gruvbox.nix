{
  config,
  lib,
  ...
}:
{
  theme = lib.mkMerge [
    (lib.mkIf (config.nx.desktop.theme == "gruvbox-light") {
      name = "gruvbox-light";
      opencodename = "gruvbox";
      type = "light";
      foreground = "#3c3836";
      primary = "#79740e";
      secondary = "#af3a03";
      red = "#9d0006";
      orange = "#af3a03";
      yellow = "#b57614";
      green = "#79740e";
      aqua = "#427b58";
      blue = "#076678";
      purple = "#8f3f71";
      grey0 = "#928374";
      grey1 = "#7c6f64";
      grey2 = "#665c54";
      statusline1 = "#076678";
      statusline2 = "#928374";
      statusline3 = "#9d0006";
      bg_dim = "#f9f5d7";
      bg0 = "#fbf1c7";
      bg1 = "#ebdbb2";
      bg2 = "#d5c4a1";
      bg3 = "#bdae93";
      bg4 = "#a89984";
      bg5 = "#928374";
      bg_visual = "#98971a";
      bg_red = "#cc241d";
      bg_green = "#98971a";
      bg_blue = "#458588";
      bg_yellow = "#d79921";
    })
    (lib.mkIf (config.nx.desktop.theme == "gruvbox-dark") {
      name = "gruvbox-dark";
      opencodename = "gruvbox";
      type = "dark";
      foreground = "#ebdbb2";
      primary = "#458588";
      secondary = "#d79921";
      red = "#cc241d";
      orange = "#d65d0e";
      yellow = "#d79921";
      green = "#98971a";
      aqua = "#689d6a";
      blue = "#83a598";
      purple = "#b16286";
      grey0 = "#928374";
      grey1 = "#7c6f64";
      grey2 = "#665c54";
      statusline1 = "#83a598";
      statusline2 = "#928374";
      statusline3 = "#cc241d";
      bg_dim = "#282828";
      bg0 = "#282828";
      bg1 = "#3c3836";
      bg2 = "#504945";
      bg3 = "#665c54";
      bg4 = "#7c6f64";
      bg5 = "#928374";
      bg_visual = "#3c3836";
      bg_red = "#fb4934";
      bg_green = "#b8bb26";
      bg_blue = "#83a598";
      bg_yellow = "#fabd2f";
    })
  ];
}
