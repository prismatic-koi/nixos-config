{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.discord.enable = lib.mkEnableOption "enables discord" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.discord.enable {
    home-manager.users.${config.nx.username} = hm: {
      programs.discord = {
        enable = true;
        package = pkgs.discord;
      };
      # Discord replaces the settings.json symlink with a regular file at runtime.
      home.file."${hm.config.xdg.configHome}/${hm.config.programs.discord.configName}/settings.json".force =
        true;
      home.persistence."/persist" = {
        directories = [
          ".config/discord"
        ];
      };
      wayland.windowManager.hyprland.settings =
        lib.mkIf (config.home-manager.users.${config.nx.username}.wayland.windowManager.hyprland.enable)
          {
            window_rule = [
              # silently open on workspace 2
              {
                match.class = "discord";
                workspace = "2 silent";
              }
            ];
          };
    };
  };
}
