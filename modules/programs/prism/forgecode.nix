{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.forgecode.enable = lib.mkEnableOption "enables forgecode AI coding agent" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.prism.forgecode.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = [ pkgs.forgecode ];
      # Persist all forgecode state — credentials, config, conversation history,
      # snapshots, cache, etc. all live under ~/forge/ (the base_path).
      home.persistence."/persist" = {
        directories = [
          "forge"
        ];
      };
    };
  };
}
