{
  config,
  lib,
  pkgs,
  ...
}:
let
  nasServerIP = "10.87.42.200";
in
{
  options = {
    nx.system.nfs-mounts.enable =
      lib.mkEnableOption "Set up local NFS mounts (local machines only)"
      // {
        default = false;
      };
  };
  config = lib.mkIf (config.nx.system.nfs-mounts.enable && pkgs.stdenv.isLinux) {
    fileSystems."/nfs/3d" = {
      device = "${nasServerIP}:/volume1/3d";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
    fileSystems."/nfs/ben-backup" = {
      device = "${nasServerIP}:/volume1/ben-backup";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
    fileSystems."/nfs/books" = {
      device = "${nasServerIP}:/volume1/Books";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
    fileSystems."/nfs/downloads" = {
      device = "${nasServerIP}:/volume1/downloads";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
    fileSystems."/nfs/learn" = {
      device = "${nasServerIP}:/volume1/learn";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
    fileSystems."/nfs/longhorn-backups" = {
      device = "${nasServerIP}:/volume1/longhorn-backups";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
    fileSystems."/nfs/movies" = {
      device = "${nasServerIP}:/volume1/Films";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
    fileSystems."/nfs/music" = {
      device = "${nasServerIP}:/volume1/Music";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
    fileSystems."/nfs/tv" = {
      device = "${nasServerIP}:/volume1/TV-Series";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
    fileSystems."/nfs/software" = {
      device = "${nasServerIP}:/volume1/Software";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
    fileSystems."/nfs/photos" = {
      device = "${nasServerIP}:/volume1/photos";
      fsType = "nfs";
      options = [ "x-systemd.automount" ];
    };
  };
}
