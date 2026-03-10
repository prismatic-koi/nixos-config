{
  config,
  lib,
  ...
}:
{
  options = {
    nx.programs.mangohud.enable = lib.mkEnableOption "enables mangohud" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.mangohud.enable {
    home-manager.users.${config.nx.username} = {
      programs.mangohud = {
        enable = true;
        settings = {
          gpu_temp = true;
        };
      };
    };
  };
}
