{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.gimp.enable = lib.mkEnableOption "enables gimp" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.gimp.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        gimp
      ];
    };
  };
}
