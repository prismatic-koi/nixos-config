{
  config,
  lib,
  ...
}:
let
  homeDir = config.home-manager.users.ben.home.homeDirectory;
in
{
  options = {
    nx.programs.nh.enable = lib.mkEnableOption "enables nix helper tool" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.nh.enable {
    programs.nh = {
      enable = true;
      flake = "${homeDir}/code/nixos-config";
    };
  };
}
