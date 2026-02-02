{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.services.pipewire.enable = lib.mkEnableOption "Enable pipewire for audio" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.services.pipewire.enable && pkgs.stdenv.isLinux) {
    services.pipewire = {
      enable = true;
      pulse.enable = true;
      alsa = {
        enable = true;
        support32Bit = true;
      };
      audio.enable = true;
      wireplumber.enable = true;
    };
    security.rtkit.enable = true;
    environment.systemPackages = with pkgs; [
      qpwgraph
    ];
  };
}
