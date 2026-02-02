{
  config,
  pkgs,
  lib,
  ...
}:
with config.theme;
{
  imports = [
    # rofi launcher scripts
    ./add-to-daily-todo.nix
    ./add-to-shopping-list.nix
    ./application-launcher.nix
    ./emoji-picker.nix
    ./nvim-session-launcher.nix
    ./rofi-calculator.nix
    ./script-launcher.nix
    # rofi themeing (theme.rasi)
    ./theme.nix
  ];
  options = {
    nx.desktop.rofi.enable = lib.mkEnableOption "enables rofi" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.desktop.rofi.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben = {
      programs.rofi = {
        enable = true;
        package = pkgs.rofi;
        font = "Noto Sans 14";
        plugins = with pkgs; [
          rofi-calc
          rofi-emoji
        ];
        extraConfig = {
          steal-focus = true;
          show-icons = true;
          icon-theme = "Papirus-Dark";
          application-fallback-icon = "run-build";
          drun-display-format = "{icon} {name}";
          matching = "fuzzy";
          scroll-method = 0;
          disable-history = false;
          display-drun = "";
          display-windows = "Windows:";
          display-run = " ";
          sort = true;
          sorting-method = "fzf";
        };
        theme = "~/.config/rofi/theme.rasi";
      };

      # my scripts relevant to rofi
      home.sessionPath = [ "$HOME/.local/scripts" ];
    };
  };
}
