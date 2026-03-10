{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.golang.enable = lib.mkEnableOption "enables go development environment" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.golang.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        go
        gopls
      ];
    };
  };
}
