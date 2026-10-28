{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.claude-desktop.enable =
      lib.mkEnableOption "enables Claude Desktop (official .deb build)"
      // {
        default = false;
      };
  };
  config = lib.mkIf config.nx.programs.claude-desktop.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = [ pkgs.claude-desktop ];
      home.persistence."/persist" = {
        directories = [
          ".config/Claude"
        ];
      };
    };
  };
}
