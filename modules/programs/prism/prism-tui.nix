{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
let
  prismConfig = {
    color_primary = primary;
    color_secondary = secondary;
    color_purple = purple;
    color_yellow = yellow;
    color_green = green;
    color_blue = blue;
    color_red = red;
    color_foreground = foreground;
    color_bg0 = bg0;
    kitty_bin = "${pkgs.kitty}/bin/kitty";
    container_mode = false;
    sidecar_plugin_path = "${
      config.home-manager.users.${config.nx.username}.xdg.configHome
    }/opencode/plugins/prism-hooks.ts";
    worktree_exclude = config.nx.programs.prism.worktreeExclude;
    project_locations = config.nx.programs.prism.projects.locations;
    project_specific = config.nx.programs.prism.projects.specific;
  };
in
{
  options = {
    nx.programs.prism.tui.enable = lib.mkEnableOption "enables prism Go TUI binary" // {
      default = true;
    };
  };

  config = lib.mkIf (config.nx.programs.prism.tui.enable && config.nx.programs.prism.enable) {
    home-manager.users.${config.nx.username} = {
      home.packages = [
        (pkgs.callPackage ../../../pkgs/prism.nix { })
      ];

      xdg.configFile."prism/config.json".text = builtins.toJSON prismConfig;

      programs.zsh.shellAliases = {
        gwc = "prism clone";
      };
    };
  };
}
