{
  config,
  pkgs,
  lib,
  ...
}:
with config.theme;
{
  options = {
    nx.desktop.swaylock.enable = lib.mkEnableOption "enables swaylock" // {
      # default to enable if sway is
      default = config.nx.desktop.sway.enable;
    };
  };
  config = lib.mkIf (config.nx.desktop.swaylock.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben.programs.swaylock = {
      enable = true;
      settings = {
        color = "000000";
        font = "JetBrainsMono Nerd Font, monospace";
        font-size = 20;
        disable-caps-lock-text = true;
        indicator-caps-lock = true;
        ignore-empty-passwor = true;
        show-failed-attempts = true;
        line-uses-inside = true;
        indicator-radius = 70;
        indicator-thickness = 15;
        inside-color = "${bg0}ff";
        inside-ver-color = "${bg0}ff";
        inside-wrong-color = "${bg0}ff";
        inside-clear-color = "${bg0}ff";
        inside-caps-lock-color = "${bg0}ff";
        text-color = "${bg0}ff";
        text-clear-color = "${orange}ff";
        text-ver-color = "${blue}ff";
        text-wrong-color = "${red}ff";
        text-caps-lock-color = "${red}ff";
        layout-bg-color = "${bg_dim}ff";
        layout-text-color = "${bg0}ff";
        line-color = "${bg0}ff";
        line-clear-color = "${bg0}ff";
        line-caps-lock-color = "${bg0}ff";
        line-ver-color = "${bg0}ff";
        line-wrong-color = "${bg0}ff";
        separator-color = "${bg0}ff";
        ring-color = "${blue}ff";
        ring-clear-color = "${orange}ff";
        ring-caps-lock-color = "${red}ff";
        ring-ver-color = "${blue}ff";
        ring-wrong-color = "${red}ff";
        bs-hl-color = "${purple}ff";
        key-hl-color = "${green}ff";
      };
    };
  };
}
