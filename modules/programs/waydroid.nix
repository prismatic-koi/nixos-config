{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.waydroid.enable = lib.mkEnableOption "enables waydroid" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.waydroid.enable (
    # Waydroid is Linux-only (Android emulation via Linux kernel features)
    lib.mkIf pkgs.stdenv.isLinux {
      virtualisation = {
        waydroid.enable = true;
      };
      environment.systemPackages = with pkgs; [
        # for clipboard sharing
        (python3.withPackages (ps: with ps; [ pyclip ]))
      ];

      environment.persistence."/persist/system" = {
        hideMounts = true;
        directories = [
          "/var/lib/waydroid"
        ];
      };

      # Redirect waydroid user data to persistent storage directly
      systemd.services.waydroid-container.environment = {
        XDG_DATA_HOME = "/persist/home/ben/.local/share";
      };

      # home-manager.users.${config.nx.username} = {
      #   home.persistence."/persist" = {
      #     directories = [
      #       ".local/share/waydroid"
      #     ];
      #   };
      # };
    }
  );
}
