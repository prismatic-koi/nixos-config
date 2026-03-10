{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.signal.enable = lib.mkEnableOption "enables signal-desktop" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.signal.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        signal-desktop
      ];
      home.persistence."/persist" = {
        directories = [
          ".config/Signal"
        ];
      };
    };
  };
}
