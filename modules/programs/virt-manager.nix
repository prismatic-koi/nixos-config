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
    # libvirt 12.1.0 added LoadCredentialEncrypted to the libvirtd unit, which
    # requires /var/lib/systemd/credential.secret to survive reboots. This is
    # incompatible with impermanence. Override the unit to drop the credential
    # loading, and disable libvirt's secret encryption in secret.conf to match.
    systemd.services.libvirtd.serviceConfig.LoadCredentialEncrypted = lib.mkForce "";
    environment.etc."libvirt/secret.conf".text = ''
      encrypt_data = 0
    '';
    users.users.${config.nx.username}.extraGroups = [
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
