{
  config,
  lib,
  pkgs,
  inputs,
  isLinux,
  ...
}:
let
  username = config.nx.username;
  homeDir = config.home-manager.users.${username}.home.homeDirectory;
in
{
  # Import sops modules - NixOS module for Linux only (Darwin uses home-manager sops module)
  imports = lib.optionals isLinux [ inputs.sops-nix.nixosModules.sops ];

  options = {
    nx.system.sops.enable = lib.mkEnableOption "Enable sops module" // {
      default = true;
    };
    nx.system.sops.ageKeys.enable = lib.mkEnableOption "Add Age encryption keys to machine" // {
      default = false;
    };
  };

  config = lib.mkIf config.nx.system.sops.enable (
    lib.mkMerge [
      # Darwin: disable system-level sops, set a dummy keyFile to satisfy assertions
      (lib.mkIf pkgs.stdenv.isDarwin {
        sops.age.keyFile = lib.mkForce "${homeDir}/.config/sops/age/keys.txt";
      })

      # NixOS system-level sops configuration
      (lib.mkIf pkgs.stdenv.isLinux {
        # general sops module options
        sops.age = {
          keyFile = "/var/lib/sops-nix/key.txt";
          generateKey = true;
          # this needs to be the /persist path,
          # or else it wont be available when needed to create user passwords etc
          sshKeyPaths = [ "/persist/system/etc/ssh/nix-ed25519" ];
        };
        sops.secrets =
          let
            sopsFile = ./secrets/age.sops.yaml;
          in
          {
            "age/personal" = {
              owner = username;
              mode = "0600";
              path = "${homeDir}/.config/sops/age/keys.txt";
              sopsFile = sopsFile;
            };
          };
        system.activationScripts.homeAgeKeysFolderPermissions = ''
          mkdir -p ${homeDir}/.config/sops/age
          chown ${username}:users ${homeDir}/.config/sops/age
        '';
        environment.sessionVariables = {
          SOPS_AGE_KEY_FILE = "${homeDir}/.config/sops/age/keys.txt";
        };
      })

      # Darwin home-manager sops configuration
      (lib.mkIf pkgs.stdenv.isDarwin {
        home-manager.users.${username} = {
          imports = [
            inputs.sops-nix.homeManagerModules.sops
          ];

          sops.age = {
            keyFile = "${homeDir}/.config/sops/age/keys.txt";
            sshKeyPaths = [ "${homeDir}/.ssh/nix-ed25519" ];
          };

          # TODO: Temporary workaround for https://github.com/Mic92/sops-nix/issues/890
          # The LaunchAgent on macOS has an empty PATH when no age plugins are configured,
          # causing sops-install-secrets to fail finding 'getconf' at /usr/bin/getconf.
          # This can be removed once https://github.com/Mic92/sops-nix/pull/891 is merged
          # and we update sops-nix. Check if this is still needed and remove if possible.
          launchd.agents.sops-nix = {
            enable = true;
            config = {
              EnvironmentVariables = {
                PATH = lib.mkForce "/usr/bin:/bin:/usr/sbin:/sbin";
              };
            };
          };
        };
      })

      # Common configuration for both platforms
      {
        home-manager.users.${username}.home.sessionVariables = {
          SOPS_AGE_KEY_FILE = "${homeDir}/.config/sops/age/keys.txt";
        };
      }
    ]
  );
}
