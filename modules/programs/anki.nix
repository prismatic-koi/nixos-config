{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.anki.enable = lib.mkEnableOption "enables anki" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.anki.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        anki
      ];
      home.persistence."/persist" = {
        directories = [
          ".local/share/Anki2"
        ];
      };
    };
  };
}
