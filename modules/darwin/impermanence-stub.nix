{
  config,
  lib,
  ...
}:
{
  options = {
    # Stub for darwin compatibility: home.persistence
    # On NixOS systems with impermanence enabled, this option is used to configure
    # which home directories and files are persisted across reboots. On darwin,
    # we provide this as a no-op to maintain configuration compatibility across
    # both platforms, preventing errors when shared modules reference this option.
    home.persistence = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule {
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
        };
      });
      default = { };
      description = ''
        Stub implementation of home.persistence for darwin compatibility.
        On NixOS, this configures persistent home directories via impermanence.
        On darwin, this option is provided as a no-op to allow unified configurations.
      '';
    };

    # Stub for darwin compatibility: environment.persistence
    # On NixOS systems with impermanence enabled, this option is used to configure
    # which system directories and files are persisted across reboots. On darwin,
    # we provide this as a no-op to maintain configuration compatibility across
    # both platforms, preventing errors when shared modules reference this option.
    environment.persistence = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule {
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
      });
      default = { };
      description = ''
        Stub implementation of environment.persistence for darwin compatibility.
        On NixOS, this configures persistent system directories via impermanence.
        On darwin, this option is provided as a no-op to allow unified configurations.
      '';
    };
  };

  config = {
    # No actual configuration needed - this is purely a stub for compatibility
  };
}
