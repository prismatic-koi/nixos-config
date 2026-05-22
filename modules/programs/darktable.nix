{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.darktable.enable = lib.mkEnableOption "enables darktable" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.darktable.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        darktable
      ];
      home.persistence."/persist" = {
        directories = [
          ".config/darktable"
        ];
      };
      wayland.windowManager.hyprland.settings =
        lib.mkIf (config.home-manager.users.${config.nx.username}.wayland.windowManager.hyprland.enable)
          {
            window_rule = [
              # darktable splash screen
              {
                match.title = "darktable starting";
                float = true;
              }
              # prevent darktable from maximising on start
              {
                match.class = "darktable";
                suppress_event = "fullscreen";
              }
            ];
          };
    };
  };
}
