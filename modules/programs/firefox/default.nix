{
  config,
  pkgs,
  lib,
  ...
}:
let
  homeDir = config.home-manager.users.${config.nx.username}.home.homeDirectory;
in
{
  imports = [
    ./home-page.nix
    ./tridactyl.nix
    ./tridactyl-style.nix
    ./user-chrome.nix
    ./user-content.nix
  ];
  options = {
    nx.programs.firefox.enable = lib.mkEnableOption "enables firefox" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.firefox.enable {
    home-manager.users.${config.nx.username} = {
      programs.firefox = {
        # NOTE: On macOS, programs.firefox works but may have limitations.
        # If issues arise, Firefox can be installed via Homebrew while
        # keeping this module enabled for configuration management.
        enable = true;
        nativeMessagingHosts = [
          pkgs.tridactyl-native
        ];
        profiles = {
          main = {
            id = 0;
            name = "ben";
            isDefault = true;
            settings = {
              "signon.rememberSignons" = false; # Disable built-in password manager
              "browser.startup.homepage" = "${homeDir}/.config/tridactyl/home.html"; # custom homepage
              "browser.compactmode.show" = true;
              "browser.uidensity" = 1; # enable compact mode
              "browser.aboutConfig.showWarning" = false;
              "browser.tabs.firefox-view" = false;
              "browser.download.folderList" = 2; # 2 = custom location
              "browser.download.dir" = "${homeDir}/downloads";
              "browser.urlbar.update2.engineAliasRefresh" = true; # allow search engine updates
              "browser.sessionstore.resume_from_crash" = false; # disable session restore after crash (its just a shutdown)
              "toolkit.legacyUserProfileCustomizations.stylesheets" = true;
              "ui.systemUsesDarkTheme" = if config.theme.type == "dark" then 1 else 0;
              "extensions.pocket.enabled" = false;

              # disable some gestures
              "browser.gesture.swipe.left" = "";
              "browser.gesture.swipe.right" = "";

              # dubious performance enhancements
              "network.buffer.cache.size" = 524288; # 512KB (default is 32 kB)
              "network.http.max-connections" = 1800; # default is 900
              "network.http.max-persistent-connections-per-server" = 12; # default is 6
              "network.http.max-urgent-start-excessive-connections-per-host" = 10; # default is 3
              "network.http.pacing.requests.burst" = 32; # default is 10
              "network.http.pacing.requests.min-parallelism" = 10; # default is 6
              "network.websocket.max-connections" = 400; # default is 200
              "network.ssl_tokens_cache_capacity" = 32768; # more TLS token caching (fast reconnects) (default is 2048)
              "image.mem.decode_bytes_at_a_time" = 65536; # 64KB (default is 16KB)

              # Enable installing non signed extensions
              "extensions.langpacks.signatures.required" = false;
              "xpinstall.signatures.required" = false;

              # Enable userChrome editor (Ctrl+Shift+Alt+I)
              "devtools.chrome.enabled" = true;
              "devtools.debugger.remote-enabled" = true;
            };
          };
        };
      };
      xdg.mimeApps.defaultApplications = lib.mkIf (config.nx.programs.defaultWebBrowser == "firefox") {
        "text/html" = [ "firefox.desktop" ];
        "text/xml" = [ "firefox.desktop" ];
        "x-scheme-handler/http" = [ "firefox.desktop" ];
        "x-scheme-handler/https" = [ "firefox.desktop" ];
      };
      home.persistence."/persist" = {
        directories = [
          ".mozilla/firefox"
        ];
      };
    };
  };
}
