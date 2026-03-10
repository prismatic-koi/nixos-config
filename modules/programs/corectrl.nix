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
    users.users.${config.nx.username}.extraGroups = [ "corectrl" ];
    home-manager.users.${config.nx.username} = {
      home.persistence."/persist" = {
        directories = [ ".config/corectrl" ];
      };
    };
  };
}
