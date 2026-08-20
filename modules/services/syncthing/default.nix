{
  config,
  lib,
  pkgs,
  ...
}:
let
  username = config.nx.username;
  homeDir = config.home-manager.users.${username}.home.homeDirectory;
in
{
  imports = [
    ./secrets.nix
  ];
  options = {
    nx.services.syncthing.enable =
      lib.mkEnableOption "Set up syncthing (includes documents folder)"
      // {
        default = false;
      };
    nx.services.syncthing.databaseDir = lib.mkOption {
      type = lib.types.str;
      default =
        if pkgs.stdenv.hostPlatform.isDarwin then
          "${homeDir}/.local/state/syncthing"
        else
          "/persist/cache/syncthing";
      description = "Location for syncthing database";
    };
    nx.services.syncthing.configDir = lib.mkOption {
      type = lib.types.str;
      default =
        if pkgs.stdenv.hostPlatform.isDarwin then
          "${homeDir}/Library/Application Support/Syncthing"
        else
          "/persist/home/${username}/.config/syncthing";
      description = "Location for syncthing config";
    };
    nx.services.syncthing.obsidian.enable = lib.mkEnableOption "Set up syncthing obsidian folder" // {
      default = false;
    };
    nx.services.syncthing.obsidian.path = lib.mkOption {
      type = lib.types.str;
      default =
        if pkgs.stdenv.hostPlatform.isDarwin then
          "${homeDir}/Documents/obsidian"
        else
          "/persist/home/${username}/documents/obsidian";
      description = "Location for obsidian folder";
    };
    nx.services.syncthing.calibre.enable = lib.mkEnableOption "Set up syncthing calibre folder" // {
      default = false;
    };
    nx.services.syncthing.calibre.path = lib.mkOption {
      type = lib.types.str;
      default =
        if pkgs.stdenv.hostPlatform.isDarwin then
          "${homeDir}/Documents/calibre"
        else
          "/persist/home/${username}/documents/calibre";
      description = "Location for calibre folder";
    };
    nx.services.syncthing.music.enable = lib.mkEnableOption "Set up syncthing music folder" // {
      default = false;
    };
    nx.services.syncthing.music.path = lib.mkOption {
      type = lib.types.str;
      default =
        if pkgs.stdenv.hostPlatform.isDarwin then
          "${homeDir}/Music"
        else
          "/persist/home/${username}/music/";
      description = "Location for music folder";
    };
    nx.services.syncthing.photos.enable = lib.mkEnableOption "Set up syncthing photos folder" // {
      default = false;
    };
    nx.services.syncthing.photos.path = lib.mkOption {
      type = lib.types.str;
      default =
        if pkgs.stdenv.hostPlatform.isDarwin then
          "${homeDir}/Pictures/photos"
        else
          "/persist/home/${username}/pictures/photos";
      description = "Location for photos folder";
    };
    nx.services.syncthing.darktable.enable = lib.mkEnableOption "Set up syncthing darktable folder" // {
      default = false;
    };
    nx.services.syncthing.darktable.path = lib.mkOption {
      type = lib.types.str;
      default =
        if pkgs.stdenv.hostPlatform.isDarwin then
          "${homeDir}/.config/darktable"
        else
          "/persist/home/${username}/.config/darktable";
      description = "Location for darktable folder";
    };
  };
  config = lib.mkIf config.nx.services.syncthing.enable (
    lib.mkMerge [
      # Common syncthing configuration (applies to both NixOS system-level and home-manager)
      {
        # NixOS: system-level syncthing service
        services.syncthing = lib.mkIf pkgs.stdenv.hostPlatform.isLinux {
          enable = true;
          user = username;

          # Localhost is the boundary (issue #2787). This is also
          # Syncthing's own default, but it is declared here so the
          # security property the module relies on is visible in the
          # config instead of inherited silently: the GUI and the REST
          # API — including /metrics — answer on loopback only, and
          # port 8384 is never opened in the firewall below. See
          # ./secrets.nix for why there is no GUI auth and no pinned
          # REST API key.
          guiAddress = "127.0.0.1:8384";

          # if you don't put the database and config somewhere stable
          # syncthing will panic every startup and rebuild the database or maybe remove and re-add the folder?
          # either way, its horrible and slow and this fixes it.
          databaseDir = config.nx.services.syncthing.databaseDir;
          configDir = config.nx.services.syncthing.configDir;
          overrideDevices = true;
          settings = {
            # Clear the GUI credential #2698 wrote into the runtime
            # config.xml (issue #2787). Dropping `guiPasswordFile`
            # stops this repo from SETTING a password; it clears
            # nothing that is already there, and the credential lives
            # in persisted host state, not in the store. Left alone,
            # `IsAuthEnabled()` stays true after the switch, Syncthing
            # keeps its auth middleware on /metrics, and the Alloy
            # scrape — which no longer sends a Bearer token — gets 401.
            # The Syncthing series for navi and tui would disappear
            # silently.
            #
            # Empty strings, not omission: `merge-syncthing-config`
            # emits `PATCH /rest/config/gui` only for keys present in
            # `settings`, and its `filterAttrsRecursive` drops only
            # `null` and `{}`, so "" survives into the request body.
            # PATCH merges, so `apikey`, `address`, and the TLS
            # settings are untouched. `IsAuthEnabled()` is
            # `AuthMode == LDAP || (len(User) > 0 && len(Password) > 0)`
            # (lib/config/guiconfiguration.go), so two empty strings
            # turn it off. The upstream assertion that forbids
            # `settings.gui.password` alongside `guiPasswordFile` is
            # satisfied because `guiPasswordFile` is now null.
            #
            # Linux only. The home-manager module used on m4mac sends
            # PUT, not PATCH, for these sub-options, which would
            # replace the whole `gui` object and drop its apikey and
            # address. m4mac never had GUI auth (#2698 was navi and
            # tui only), so it needs no clearing.
            gui = {
              user = "";
              password = "";
            };
            devices = {
              "k8s" = {
                id = "FZVNVGQ-6TJDJLG-DRWSAWW-AQLKQM7-U36GWON-7ZQ7CLF-32MBYFN-SFHWHAX";
              };
              "nas0" = {
                id = "7LANRKO-RRMWROL-PDMCTJX-WKSPOKO-LS3K35O-CJEMX7O-MHHIURW-GSF6FAS";
              };
            };
            folders = {
              "obsidian" = lib.mkIf config.nx.services.syncthing.obsidian.enable {
                id = "hgl5u-yejsp";
                devices = [ "k8s" ];
                path = config.nx.services.syncthing.obsidian.path;
              };
              "calibre" = lib.mkIf config.nx.services.syncthing.calibre.enable {
                id = "bny6u-oz6gf";
                devices = [ "nas0" ];
                path = config.nx.services.syncthing.calibre.path;
              };
              "music" = lib.mkIf config.nx.services.syncthing.music.enable {
                id = "dmuif-nefck";
                devices = [ "nas0" ];
                path = config.nx.services.syncthing.music.path;
              };
              "photos" = lib.mkIf config.nx.services.syncthing.photos.enable {
                id = "4ghtf-4leca";
                devices = [ "nas0" ];
                path = config.nx.services.syncthing.photos.path;
              };
              "darktable" = lib.mkIf config.nx.services.syncthing.darktable.enable {
                id = "x7g7m-4z7qg";
                devices = [ "nas0" ];
                path = config.nx.services.syncthing.darktable.path;
              };
            };
            options.urAccepted = -1;
          };
        };

        # Darwin: home-manager syncthing service
        home-manager.users.${username}.services.syncthing = lib.mkIf pkgs.stdenv.hostPlatform.isDarwin {
          enable = true;

          # Same loopback boundary as the NixOS branch above — see
          # that comment and ./secrets.nix (issue #2787).
          guiAddress = "127.0.0.1:8384";

          settings = {
            devices = {
              "k8s" = {
                id = "FZVNVGQ-6TJDJLG-DRWSAWW-AQLKQM7-U36GWON-7ZQ7CLF-32MBYFN-SFHWHAX";
              };
              "nas0" = {
                id = "7LANRKO-RRMWROL-PDMCTJX-WKSPOKO-LS3K35O-CJEMX7O-MHHIURW-GSF6FAS";
              };
            };
            folders = {
              "obsidian" = lib.mkIf config.nx.services.syncthing.obsidian.enable {
                id = "hgl5u-yejsp";
                path = config.nx.services.syncthing.obsidian.path;
                devices = [ "k8s" ];
              };
              "calibre" = lib.mkIf config.nx.services.syncthing.calibre.enable {
                id = "bny6u-oz6gf";
                path = config.nx.services.syncthing.calibre.path;
                devices = [ "nas0" ];
              };
              "music" = lib.mkIf config.nx.services.syncthing.music.enable {
                id = "dmuif-nefck";
                path = config.nx.services.syncthing.music.path;
                devices = [ "nas0" ];
              };
              "photos" = lib.mkIf config.nx.services.syncthing.photos.enable {
                id = "4ghtf-4leca";
                path = config.nx.services.syncthing.photos.path;
                devices = [ "nas0" ];
              };
              "darktable" = lib.mkIf config.nx.services.syncthing.darktable.enable {
                id = "x7g7m-4z7qg";
                path = config.nx.services.syncthing.darktable.path;
                devices = [ "nas0" ];
              };
            };
            options.urAccepted = -1;
          };
        };
      }

      # Linux-only: systemd and firewall configuration
      (lib.mkIf pkgs.stdenv.hostPlatform.isLinux {
        systemd.services.syncthing = {
          after = [ "network-online.target" ];
          wants = [ "network-online.target" ];
        };
        networking.firewall = {
          allowedTCPPorts = [ 22000 ];
          allowedUDPPorts = [
            22000
            21027
          ];
        };

        # Linux-only: activation scripts with chown
        system.activationScripts = {
          documentsFolder = lib.mkIf config.nx.services.syncthing.enable ''
            mkdir -p /home/${username}/documents
            chown ${username}:users /home/${username}/documents
          '';
          picturesFolder = lib.mkIf config.nx.services.syncthing.enable ''
            mkdir -p /home/${username}/pictures
            chown ${username}:users /home/${username}/pictures
          '';
          obsidianFolder = lib.mkIf config.nx.services.syncthing.obsidian.enable ''
            mkdir -p /home/${username}/documents/obsidian
            chown ${username}:users /home/${username}/documents/obsidian
          '';
          musicFolder = lib.mkIf config.nx.services.syncthing.music.enable ''
            mkdir -p /home/${username}/music
            chown ${username}:users /home/${username}/music
          '';
          photosFolder = lib.mkIf config.nx.services.syncthing.photos.enable ''
            mkdir -p /home/${username}/pictures/photos
            chown ${username}:users /home/${username}/pictures/photos
          '';
          darktableFolder = lib.mkIf config.nx.services.syncthing.darktable.enable ''
            mkdir -p /home/${username}/.config/darktable
            chown ${username}:users /home/${username}/.config/darktable
          '';
        };
      })

      # Darwin-only: ensure syncthing starts on login/boot
      (lib.mkIf pkgs.stdenv.hostPlatform.isDarwin {
        # Upstream's services.syncthing HM module sets domain = lib.mkDefault
        # "user", which forces LimitLoadToSessionType = "Background" (see the
        # HM launchd module). Background/user-domain agents are NOT auto-loaded
        # into the Aqua session at GUI login — macOS only auto-bootstraps
        # ~/Library/LaunchAgents/*.plist agents whose domain is "gui". A
        # Background agent only gets loaded via the explicit
        # "launchctl bootstrap user/$UID" that home-manager runs during a
        # switch, so on a fresh boot/login it is never loaded into any session
        # at all and RunAtLoad = true never gets a chance to fire. Overriding
        # domain to "gui" here wins over upstream's mkDefault, drops the
        # Background session-type constraint, and lets RunAtLoad fire at
        # GUI login.
        home-manager.users.${username}.launchd.agents.syncthing = {
          enable = true;
          domain = "gui";
          config.RunAtLoad = true;
        };
      })

      # Common: environment variables and persistence
      {
        # Set env for OBSIDIAN_VAULT_PATH when obsidian folder is enabled
        home-manager.users.${username} = {
          home.sessionVariables = lib.mkIf config.nx.services.syncthing.obsidian.enable {
            OBSIDIAN_VAULT_PATH = config.nx.services.syncthing.obsidian.path;
          };

          # persist the syncthing config with home-manager impermanence module
          # (no-op on darwin via impermanence-stub)
          home.persistence."/persist" = {
            directories = [ ".config/syncthing" ];
          };
        };
      }
    ]
  );
}
