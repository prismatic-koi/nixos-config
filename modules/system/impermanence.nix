{
  config,
  lib,
  pkgs,
  ...
}:
let
  username = config.nx.username;
in
{
  options = {
    nx.system.impermanence.enable = lib.mkEnableOption "Code to support impermanence" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.system.impermanence.enable && pkgs.stdenv.isLinux) {
    # Wipe the disk on each boot using a systemd stage 1 service.
    # Explicitly enable systemd initrd so the wipe-root service always runs,
    # regardless of whether nixpkgs auto-enables it for the current hardware layout.
    boot.initrd.systemd.enable = true;
    boot.initrd.systemd.services.wipe-root = {
      description = "Wipe root btrfs subvolume and rotate snapshots";
      wantedBy = [ "initrd.target" ];
      after = [ "dev-root_vg-root.device" ];
      requires = [ "dev-root_vg-root.device" ];
      before = [ "sysroot.mount" ];
      unitConfig.DefaultDependencies = false;
      serviceConfig.Type = "oneshot";
      script = ''
        set -euo pipefail

        mkdir -p /btrfs_tmp
        mount /dev/root_vg/root /btrfs_tmp

        if [[ -e /btrfs_tmp/root ]]; then
            mkdir -p /btrfs_tmp/old_roots
            timestamp=$(date --date="@$(stat -c %Y /btrfs_tmp/root)" "+%Y-%m-%-d_%H:%M:%S")
            mv /btrfs_tmp/root "/btrfs_tmp/old_roots/$timestamp"
        fi

        delete_subvolume_recursively() {
            IFS=$'\n'
            for i in $(${pkgs.btrfs-progs}/bin/btrfs subvolume list -o "$1" | cut -f 9- -d ' '); do
                delete_subvolume_recursively "/btrfs_tmp/$i"
            done
            ${pkgs.btrfs-progs}/bin/btrfs subvolume delete "$1"
        }

        for i in $(find /btrfs_tmp/old_roots/ -maxdepth 1 -mtime +14 2>/dev/null || true); do
            delete_subvolume_recursively "$i"
        done

        ${pkgs.btrfs-progs}/bin/btrfs subvolume create /btrfs_tmp/root
        umount /btrfs_tmp
      '';
    };

    # system things to persist
    fileSystems."/persist".neededForBoot = true;
    fileSystems."/nix".neededForBoot = true;
    environment.persistence."/persist/system" = {
      hideMounts = true;
      directories = [
        "/etc/NetworkManager/system-connections"
        "/etc/ssh"
        "/etc/wireguard"
        "/nix/var/nix/profiles"
        "/var/lib/bluetooth"
        "/var/lib/nixos"
        "/var/lib/sops-nix"
        "/var/lib/systemd/coredump"
        "/var/lib/wgnord"
        "/var/log"
      ];
      files = [
        "/etc/machine-id"
      ];
    };
    # without these, you will get errors the first time after install
    system.activationScripts.persistDirs = ''
      mkdir -p /persist/system/var/log
      mkdir -p /persist/system/var/lib/nixos
      mkdir -p /persist/system/var/lib/wgnord
      mkdir -p /persist/system/etc/wireguard
      mkdir -p /persist/cache
      chown -R ${username}:users /persist/cache
      mkdir -p /persist/home/${username}
      mkdir -p /persist/home/${username}/.ssh
      mkdir -p /persist/home/${username}/.local/share/Steam
      chown -R ${username}:users /persist/home/${username}
    '';
    # ensure these empty directories exist
    system.activationScripts.emptyDirs = ''
      mkdir -p /home/${username}/downloads
      chown -R ${username}:users /home/${username}/downloads
    '';
    # home-manager things to persist
    home-manager.users.${username} = {
      home.persistence."/persist" = {
        directories = [
          ".local/share/nix"
          ".local/state/nix"
          ".local/state/home-manager"
          ".cache"
          "code"
          "documents"
          "games"
          "pictures"
          # mount music on all machines except navi (it uses dedicated drive for music)
          (lib.mkIf (config.networking.hostName != "navi") "music")
        ];
      };
      home.activation = {
        # ensure these empty directories exist
        emptyDirs = ''
          mkdir -p /home/${username}/downloads
        '';
      };
    };
  };
}
