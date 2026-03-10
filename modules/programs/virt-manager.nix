{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.virt-manager.enable = lib.mkEnableOption "enables virt-manager" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.virt-manager.enable {
    programs.virt-manager = {
      enable = true;
    };
    virtualisation = {
      libvirtd.enable = true;
      spiceUSBRedirection.enable = true;
    };
    # Disable systemd credential-based secret encryption introduced in libvirt
    # 12.1.0. The credential host key lives in /var/lib/systemd which is
    # populated too early for impermanence to bind-mount it, causing libvirtd
    # to fail on every boot. We don't use libvirt's persistent secret store,
    # so this feature provides no benefit.
    environment.etc."libvirt/secret.conf".text = ''
      encrypt_data = 0
    '';
    users.users.ben.extraGroups = [
      "libvirt"
    ];
    environment.persistence."/persist/system" = {
      directories = [
        "/var/lib/libvirt"
        "/var/log/libvirt"
      ];
    };
  };
}
