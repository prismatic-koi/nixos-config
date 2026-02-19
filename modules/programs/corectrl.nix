{
  config,
  lib,
  ...
}:
{
  options = {
    nx.programs.corectrl.enable = lib.mkEnableOption "enables corectrl";
  };
  config = lib.mkIf config.nx.programs.corectrl.enable {
    programs.corectrl.enable = true;
    users.users.ben.extraGroups = [ "corectrl" ];
    home-manager.users.ben = {
      home.persistence."/persist" = {
        directories = [ ".config/corectrl" ];
      };
    };
  };
}
