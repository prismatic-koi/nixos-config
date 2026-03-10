{
  config,
  lib,
  ...
}:
{
  options = {
    # Stub for darwin compatibility: environment.persistence
    # On NixOS systems with impermanence enabled, this option is used to configure
    # which system directories and files are persisted across reboots. On darwin,
    # we provide this as a no-op to maintain configuration compatibility across
    # both platforms, preventing errors when shared modules reference this option.
    environment.persistence = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            directories = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              description = "Directories to persist (darwin stub)";
            };
            files = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              description = "Files to persist (darwin stub)";
            };
            hideMounts = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = "Hide mounts (darwin stub)";
            };
          };
        }
      );
      default = { };
      description = ''
        Stub implementation of environment.persistence for darwin compatibility.
        On NixOS, this configures persistent system directories via impermanence.
        On darwin, this option is provided as a no-op to allow unified configurations.
      '';
    };

    # Stub for darwin compatibility: boot.kernel.sysctl
    # Provides no-op boot configuration options that are referenced by networking module
    boot.kernel.sysctl = lib.mkOption {
      type = lib.types.attrsOf lib.types.anything;
      default = { };
      description = "Stub for boot.kernel.sysctl (darwin compatibility)";
    };

    # Stub for darwin compatibility: boot.initrd
    # Provides no-op boot.initrd options that are referenced by impermanence module
    boot.initrd.postDeviceCommands = lib.mkOption {
      type = lib.types.lines;
      default = "";
      description = "Stub for boot.initrd.postDeviceCommands (darwin compatibility)";
    };

    boot.loader.grub = lib.mkOption {
      type = lib.types.submodule {
        options = {
          useOSProber = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          extraConfig = lib.mkOption {
            type = lib.types.lines;
            default = "";
          };
        };
      };
      default = { };
      description = "Stub for boot.loader.grub (darwin compatibility)";
    };

    # Stub for darwin compatibility: networking.networkmanager
    networking.networkmanager.enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Stub for networking.networkmanager.enable (darwin compatibility)";
    };

    # Stub for darwin compatibility: fileSystems
    fileSystems = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            neededForBoot = lib.mkOption {
              type = lib.types.bool;
              default = false;
            };
          };
        }
      );
      default = { };
      description = "Stub for fileSystems (darwin compatibility)";
    };

    # Stub for darwin compatibility: environment.sessionVariables
    # On Darwin, session variables should be set via home-manager instead
    environment.sessionVariables = lib.mkOption {
      type = lib.types.attrsOf lib.types.anything;
      default = { };
      description = "Stub for environment.sessionVariables (darwin compatibility - use home.sessionVariables instead)";
    };

    # Note: system.activationScripts is provided by nix-darwin
    # Note: sops.secrets is provided by the real sops-nix home-manager module on Darwin

    # Stub for darwin compatibility: i18n
    # On Darwin, localization is handled differently
    i18n = lib.mkOption {
      type = lib.types.submodule {
        options = {
          defaultLocale = lib.mkOption {
            type = lib.types.str;
            default = "en_US.UTF-8";
          };
          extraLocaleSettings = lib.mkOption {
            type = lib.types.attrsOf lib.types.str;
            default = { };
          };
        };
      };
      default = { };
      description = "Stub for i18n (darwin compatibility)";
    };

    # Stub for darwin compatibility: networking.firewall
    networking.firewall = lib.mkOption {
      type = lib.types.submodule {
        options = {
          trustedInterfaces = lib.mkOption {
            type = lib.types.listOf lib.types.str;
            default = [ ];
          };
        };
      };
      default = { };
      description = "Stub for networking.firewall (darwin compatibility)";
    };

    # Stub for darwin compatibility: networking.wireguard
    networking.wireguard = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          interfaces = lib.mkOption {
            type = lib.types.attrsOf lib.types.anything;
            default = { };
          };
        };
      };
      default = { };
      description = "Stub for networking.wireguard (darwin compatibility)";
    };

    # Stub for darwin compatibility: networking.vpnConnect/vpnDisconnect
    networking.vpnConnect = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Stub for networking.vpnConnect (darwin compatibility)";
    };

    networking.vpnDisconnect = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Stub for networking.vpnDisconnect (darwin compatibility)";
    };

    networking.nameservers = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Stub for networking.nameservers (darwin compatibility)";
    };

    # Stub for darwin compatibility: programs.nh
    # nh is a NixOS helper tool, not typically used on Darwin
    programs.nh = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          flake = lib.mkOption {
            type = lib.types.nullOr lib.types.str;
            default = null;
          };
        };
      };
      default = { };
      description = "Stub for programs.nh (darwin compatibility)";
    };

    # Stub for darwin compatibility: services.mpd
    # MPD (Music Player Daemon) service configuration
    services.mpd = lib.mkOption {
      type = lib.types.submodule {
        options = {
          musicDirectory = lib.mkOption {
            type = lib.types.nullOr lib.types.str;
            default = null;
          };
        };
      };
      default = { };
      description = "Stub for services.mpd (darwin compatibility)";
    };

    # Stub for darwin compatibility: services.avahi
    services.avahi = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          nssmdns4 = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          openFirewall = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
        };
      };
      default = { };
      description = "Stub for services.avahi (darwin compatibility)";
    };

    # Stub for darwin compatibility: services.blocky
    services.blocky = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          settings = lib.mkOption {
            type = lib.types.attrsOf lib.types.anything;
            default = { };
          };
        };
      };
      default = { };
      description = "Stub for services.blocky (darwin compatibility)";
    };

    # Stub for darwin compatibility: services.printing
    services.printing = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          drivers = lib.mkOption {
            type = lib.types.listOf lib.types.anything;
            default = [ ];
          };
        };
      };
      default = { };
      description = "Stub for services.printing (darwin compatibility)";
    };

    # Stub for darwin compatibility: services.greetd
    services.greetd = lib.mkOption {
      type = lib.types.attrsOf lib.types.anything;
      default = { };
      description = "Stub for services.greetd (darwin compatibility)";
    };

    # Stub for darwin compatibility: services.upower
    services.upower = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
        };
      };
      default = { };
      description = "Stub for services.upower (darwin compatibility)";
    };

    # Stub for darwin compatibility: services.pipewire
    services.pipewire = lib.mkOption {
      type = lib.types.attrsOf lib.types.anything;
      default = { };
      description = "Stub for services.pipewire (darwin compatibility)";
    };

    # Stub for darwin compatibility: services.power-profiles-daemon
    services.power-profiles-daemon.enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Stub for services.power-profiles-daemon.enable (darwin compatibility)";
    };

    # Note: services.openssh is provided by nix-darwin

    # Stub for darwin compatibility: services.syncthing
    # On Darwin, syncthing is configured via home-manager
    services.syncthing = lib.mkOption {
      type = lib.types.submodule {
        options = {
          cert = lib.mkOption {
            type = lib.types.nullOr lib.types.str;
            default = null;
          };
          key = lib.mkOption {
            type = lib.types.nullOr lib.types.str;
            default = null;
          };
        };
      };
      default = { };
      description = "Stub for services.syncthing (darwin compatibility)";
    };

    # Stub for darwin compatibility: services.udev
    services.udev = lib.mkOption {
      type = lib.types.attrsOf lib.types.anything;
      default = { };
      description = "Stub for services.udev (darwin compatibility)";
    };

    # Stub for darwin compatibility: programs.virt-manager
    # virt-manager is a Linux virtualization tool, not available on Darwin
    programs.virt-manager = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
        };
      };
      default = { };
      description = "Stub for programs.virt-manager (darwin compatibility)";
    };

    # Stub for darwin compatibility: virtualisation
    # Virtualization configuration is Linux-specific
    virtualisation = lib.mkOption {
      type = lib.types.submodule {
        options = {
          libvirtd.enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          spiceUSBRedirection.enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          waydroid.enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          podman.enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
        };
      };
      default = { };
      description = "Stub for virtualisation (darwin compatibility)";
    };

    # Stub for darwin compatibility: security.pam.loginLimits
    security.pam.loginLimits = lib.mkOption {
      type = lib.types.listOf lib.types.anything;
      default = [ ];
      description = "Stub for security.pam.loginLimits (darwin compatibility)";
    };

    # Stub for darwin compatibility: security.sudo.wheelNeedsPassword
    security.sudo.wheelNeedsPassword = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Stub for security.sudo.wheelNeedsPassword (darwin compatibility)";
    };

    # Stub for darwin compatibility: security.polkit
    security.polkit.enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Stub for security.polkit.enable (darwin compatibility)";
    };

    # Stub for darwin compatibility: security.rtkit
    security.rtkit.enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Stub for security.rtkit.enable (darwin compatibility)";
    };

    # Stub for darwin compatibility: systemd
    # systemd is Linux-only, Darwin uses launchd
    systemd = lib.mkOption {
      type = lib.types.attrsOf lib.types.anything;
      default = { };
      description = "Stub for systemd (darwin compatibility - use launchd instead)";
    };

    # Stub for darwin compatibility: hardware
    hardware = lib.mkOption {
      type = lib.types.submodule {
        options = {
          openrazer.enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          graphics = lib.mkOption {
            type = lib.types.submodule {
              options = {
                enable = lib.mkOption {
                  type = lib.types.bool;
                  default = false;
                };
                enable32Bit = lib.mkOption {
                  type = lib.types.bool;
                  default = false;
                };
              };
            };
            default = { };
            description = "Stub for hardware.graphics (darwin compatibility)";
          };
        };
      };
      default = { };
      description = "Stub for hardware (darwin compatibility)";
    };

    # Stub for darwin compatibility: programs.gamemode
    programs.gamemode = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
        };
      };
      default = { };
      description = "Stub for programs.gamemode (darwin compatibility)";
    };

    # Stub for darwin compatibility: programs.hyprland
    programs.hyprland = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
        };
      };
      default = { };
      description = "Stub for programs.hyprland (darwin compatibility)";
    };

    # Stub for darwin compatibility: programs.sway
    programs.sway = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          extraPackages = lib.mkOption {
            type = lib.types.listOf lib.types.anything;
            default = [ ];
          };
        };
      };
      default = { };
      description = "Stub for programs.sway (darwin compatibility)";
    };

    # Stub for darwin compatibility: programs.steam
    programs.steam = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          gamescopeSession.enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          protontricks.enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          package = lib.mkOption {
            type = lib.types.nullOr lib.types.anything;
            default = null;
          };
        };
      };
      default = { };
      description = "Stub for programs.steam (darwin compatibility)";
    };

    # Stub for darwin compatibility: xdg.portal
    xdg.portal = lib.mkOption {
      type = lib.types.submodule {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
          wlr.enable = lib.mkOption {
            type = lib.types.bool;
            default = false;
          };
        };
      };
      default = { };
      description = "Stub for xdg.portal (darwin compatibility)";
    };

    # Stub for darwin compatibility: sops
    # On Darwin, sops is configured via home-manager, not system-level
    sops = lib.mkOption {
      type = lib.types.submodule {
        options = {
          age = lib.mkOption {
            type = lib.types.submodule {
              options = {
                keyFile = lib.mkOption {
                  type = lib.types.nullOr lib.types.str;
                  default = null;
                };
                generateKey = lib.mkOption {
                  type = lib.types.bool;
                  default = false;
                };
                sshKeyPaths = lib.mkOption {
                  type = lib.types.listOf lib.types.str;
                  default = [ ];
                };
              };
            };
            default = { };
          };
          secrets = lib.mkOption {
            type = lib.types.attrsOf lib.types.anything;
            default = { };
          };
        };
      };
      default = { };
      description = "Stub for sops (darwin compatibility - use home-manager sops instead)";
    };
  };

  config = {
    # Inject home.persistence stub into home-manager as a module
    home-manager.sharedModules = [
      {
        options.home.persistence = lib.mkOption {
          type = lib.types.attrsOf (
            lib.types.submodule {
              options = {
                directories = lib.mkOption {
                  type = lib.types.listOf lib.types.anything;
                  default = [ ];
                  description = "Directories to persist (darwin stub)";
                };
                files = lib.mkOption {
                  type = lib.types.listOf lib.types.str;
                  default = [ ];
                  description = "Files to persist (darwin stub)";
                };
              };
            }
          );
          default = { };
          description = ''
            Stub implementation of home.persistence for darwin compatibility.
            On NixOS, this configures persistent home directories via impermanence.
            On darwin, this option is provided as a no-op to allow unified configurations.
          '';
        };
      }
    ];
  };
}
