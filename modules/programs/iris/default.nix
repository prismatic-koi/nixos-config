{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.nx.programs.iris;
in
{
  options = {
    nx.programs.iris = {
      enable = lib.mkEnableOption "iris — daemon-mode successor to prism (codename, D-2+)" // {
        default = false;
      };
    };
  };

  config = lib.mkIf cfg.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = [ pkgs.iris ];
    };
  };
}
