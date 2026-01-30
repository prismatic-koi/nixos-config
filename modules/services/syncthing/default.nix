{
  config,
  lib,
  pkgs,
  ...
}:
let
  homeDir = config.home-manager.users.ben.home.homeDirectory;
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
      default = if pkgs.stdenv.isDarwin then "${homeDir}/.local/state/syncthing" else "/persist/cache/syncthing";
      description = "Location for syncthing database";
    };
    nx.services.syncthing.configDir = lib.mkOption {
      type = lib.types.str;
      default = if pkgs.stdenv.isDarwin then "${homeDir}/.config/syncthing" else "/persist/home/ben/.config/syncthing";
      description = "Location for syncthing config";
    };
    nx.services.syncthing.obsidian.enable = lib.mkEnableOption "Set up syncthing obsidian folder" // {
      default = false;
    };
    nx.services.syncthing.obsidian.path = lib.mkOption {
      type = lib.types.str;
      default = if pkgs.stdenv.isDarwin then "${homeDir}/Documents/obsidian" else "/persist/home/ben/documents/obsidian";
      description = "Location for obsidian folder";
    };
    nx.services.syncthing.calibre.enable = lib.mkEnableOption "Set up syncthing calibre folder" // {
      default = false;
    };
    nx.services.syncthing.calibre.path = lib.mkOption {
      type = lib.types.str;
      default = if pkgs.stdenv.isDarwin then "${homeDir}/Documents/calibre" else "/persist/home/ben/documents/calibre";
      description = "Location for calibre folder";
    };
    nx.services.syncthing.music.enable = lib.mkEnableOption "Set up syncthing music folder" // {
      default = false;
    };
    nx.services.syncthing.music.path = lib.mkOption {
      type = lib.types.str;
      default = if pkgs.stdenv.isDarwin then "${homeDir}/Music" else "/persist/home/ben/music/";
      description = "Location for music folder";
    };
    nx.services.syncthing.photos.enable = lib.mkEnableOption "Set up syncthing photos folder" // {
      default = false;
    };
    nx.services.syncthing.photos.path = lib.mkOption {
      type = lib.types.str;
      default = if pkgs.stdenv.isDarwin then "${homeDir}/Pictures/photos" else "/persist/home/ben/pictures/photos";
      description = "Location for photos folder";
    };
    nx.services.syncthing.darktable.enable = lib.mkEnableOption "Set up syncthing darktable folder" // {
      default = false;
    };
    nx.services.syncthing.darktable.path = lib.mkOption {
      type = lib.types.str;
      default = if pkgs.stdenv.isDarwin then "${homeDir}/.config/darktable" else "/persist/home/ben/.config/darktable";
      description = "Location for darktable folder";
    };
  };
  config = lib.mkIf config.nx.services.syncthing.enable {
    services.syncthing = {
      enable = true;
      user = "ben";

      # if you don't put the database and config somewhere stable
      # syncthing will panic every startup and rebuild the database or maybe remove and re-add the folder?
      # either way, its horrible and slow and this fixes it.
      databaseDir = config.nx.services.syncthing.databaseDir;
      configDir = config.nx.services.syncthing.configDir;
      overrideDevices = true;
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

    # Linux-only: systemd and firewall configuration
    systemd.services.syncthing = lib.mkIf pkgs.stdenv.isLinux {
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
    };
    networking.firewall = lib.mkIf pkgs.stdenv.isLinux {
      allowedTCPPorts = [
        22000
      ];
      allowedUDPPorts = [
        22000
        21027
      ];
    };

    # Linux-only: activation scripts with chown
    system.activationScripts = lib.mkIf pkgs.stdenv.isLinux {
      documentsFolder = lib.mkIf config.nx.services.syncthing.enable ''
        mkdir -p /home/ben/documents
        chown ben:users /home/ben/documents
      '';
      picturesFolder = lib.mkIf config.nx.services.syncthing.enable ''
        mkdir -p /home/ben/pictures
        chown ben:users /home/ben/pictures
      '';
      obsidianFolder = lib.mkIf config.nx.services.syncthing.obsidian.enable ''
        mkdir -p /home/ben/documents/obsidian
        chown ben:users /home/ben/documents/obsidian
      '';
      musicFolder = lib.mkIf config.nx.services.syncthing.music.enable ''
        mkdir -p /home/ben/music
        chown ben:users /home/ben/music
      '';
      photosFolder = lib.mkIf config.nx.services.syncthing.photos.enable ''
        mkdir -p /home/ben/pictures/photos
        chown ben:users /home/ben/pictures/photos
      '';
      darktableFolder = lib.mkIf config.nx.services.syncthing.darktable.enable ''
        mkdir -p /home/ben/.config/darktable
        chown ben:users /home/ben/.config/darktable
      '';
    };

    # Set env for OBSIDIAN_VAULT_PATH when obsidian folder is enabled
    home-manager.users.ben.home.sessionVariables =
      lib.mkIf config.nx.services.syncthing.obsidian.enable
        {
          OBSIDIAN_VAULT_PATH = config.nx.services.syncthing.obsidian.path;
        };

    # persist the syncthing config with home-manager impermanence module
    # (no-op on darwin via impermanence-stub)
    home-manager.users.ben.home.persistence."/persist" = {
      directories = [
        ".config/syncthing"
      ];
    };
  };
}
