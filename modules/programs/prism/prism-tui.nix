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
        # Build with theme colours and project config injected via ldflags
        (pkgs.callPackage ../../../pkgs/prism.nix {
          colorPrimary = primary;
          colorSecondary = secondary;
          colorPurple = purple;
          colorYellow = yellow;
          colorGreen = green;
          colorBlue = blue;
          colorRed = red;
          colorForeground = foreground;
          colorBg0 = bg0;
          worktreeExclude = config.nx.programs.prism._internal.worktreeExcludeList;
          projectLocations = config.nx.programs.prism._internal.projectLocationsList;
          projectSpecific = config.nx.programs.prism._internal.projectSpecificList;
        })
      ];
      programs.zsh.shellAliases = {
        gwc = "prism clone";
      };
    };
  };
}
