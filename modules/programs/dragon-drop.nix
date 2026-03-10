{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.dragon-drop.enable = lib.mkEnableOption "enables dragon-drop" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.programs.dragon-drop.enable) {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [ dragon-drop ];
      # lf integration
      programs.lf.extraConfig = lib.mkIf config.nx.programs.lf.enable ''
        # dragon
        cmd dragon-out %dragon-drop -a -x "$fx"
        cmd dragon-in ''${{
          files=$(dragon-drop -t -x)
          for file in $files
          do
            path=''${file#file://}
            name=$(basename "$path")
            mv "$path" "$(pwd)/$name"
          done
        }}
        map do dragon-out
        map di dragon-in
      '';
    };
  };
}
