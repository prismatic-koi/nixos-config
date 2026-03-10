{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.plexamp.enable = lib.mkEnableOption "enables plexamp" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.plexamp.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        plexamp
      ];
      home.persistence."/persist" = {
        directories = [
          ".config/Plexamp"
        ];
      };
    };
  };
}
