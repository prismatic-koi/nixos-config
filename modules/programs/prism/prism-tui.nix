{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
{
  options = {
    nx.programs.prism.tui.enable = lib.mkEnableOption "enables prism Go TUI binary" // {
      default = true;
    };
  };

  config = lib.mkIf (config.nx.programs.prism.tui.enable && config.nx.programs.prism.enable) {
    home-manager.users.${config.nx.username} = {
      home.packages = [
        # Build with theme colours injected via ldflags so the TUI matches the active theme
        (pkgs.callPackage ../../../pkgs/prism.nix {
          colorPrimary = primary;
          colorSecondary = secondary;
          colorPurple = purple;
          colorYellow = yellow;
          colorGreen = green;
          colorBlue = blue;
          colorRed = red;
        })
      ];
    };
  };
}
