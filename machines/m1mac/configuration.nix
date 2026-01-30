{
  pkgs,
  inputs,
  config,
  lib,
  ...
}:
{
  imports = [
    ../../modules
  ];

  users.users.ben = {
    home = "/Users/ben";
  };

  networking.hostName = "m1mac";

  # Module configuration using nx namespace (matching NixOS pattern)
  nx = {
    desktop = {
      theme = "everforest";
    };
    programs = {
      gcalcli.enable = true;
      homeAutomation.enable = true;
      qutebrowser.enable = true;
    };
    services = {
      syncthing = {
        enable = true;
        obsidian.enable = true;
      };
    };
  };

  # Darwin-specific packages not in shared modules
  environment.systemPackages = with pkgs; [
    arping
    auth0-cli
    azure-cli
    gnutar
    go
    jankyborders
    podman # darwin doesn't use virtualisation.podman
    rustup
    ssm-session-manager-plugin
    tree
    tridactyl-native
    utm
  ];

  fonts.packages = with pkgs; [
    nerd-fonts.jetbrains-mono
    noto-fonts
    noto-fonts-color-emoji
  ];

  security.sudo.extraConfig = ''
    ben ALL=(ALL:ALL) NOPASSWD: ALL
  '';

  services = {
    karabiner-elements.enable = false;
  };

  system.primaryUser = "ben";

  system.defaults = {
    finder = {
      AppleShowAllExtensions = true;
      AppleShowAllFiles = true;
    };
    dock = {
      autohide = true;
      # basically, permanantly hide dock
      autohide-delay = 1000.0;
      orientation = "left";
    };
    menuExtraClock = {
      IsAnalog = false;
      Show24Hour = true;
      ShowAMPM = false;
      ShowSeconds = true;
    };
    spaces = {
      spans-displays = false;
    };
    universalaccess = {
      reduceMotion = true;
      reduceTransparency = true;
    };
    NSGlobalDomain = {
      _HIHideMenuBar = false;
      InitialKeyRepeat = 14;
      KeyRepeat = 1;
      AppleInterfaceStyle = "Dark";
      AppleICUForce24HourTime = true;
      AppleMeasurementUnits = "Centimeters";
      AppleMetricUnits = 1;
      AppleTemperatureUnit = "Celsius";
      NSWindowShouldDragOnGesture = true;
      NSAutomaticWindowAnimationsEnabled = false;
    };
    # Fix CMD+Q not working in Electron apps (Plexamp)
    # This is a workaround for Electron apps not properly handling macOS keyboard shortcuts
    # See: https://github.com/electron/electron/issues/7165
    CustomUserPreferences = {
      "tv.plex.plexamp" = {
        NSUserKeyEquivalents = {
          "Quit Plexamp" = "@q";
        };
      };
    };
  };

  homebrew = {
    enable = true;
    onActivation = {
      autoUpdate = true;
      cleanup = "uninstall";
      upgrade = true;
    };
    brews = [
      "node"
      "ripgrep" # for plenary in neovim, it can't find the nix binary
      "python"
      "vfkit"
    ];
    casks = [
      "1password"
      "1password-cli"
      "bitwarden"
      "firefox"
      "karabiner-elements"
      "nikitabobko/tap/aerospace"
      "qutebrowser"
      "raycast"
      "scroll-reverser"
    ];
  };

  # Activation scripts
  system.activationScripts.extraActivation.text = ''
    # Set Cmd+Q shortcut for Plexamp (Electron app workaround)
    # Run as user since defaults needs to write to user preferences
    sudo -u ben /usr/bin/defaults write tv.plex.plexamp NSUserKeyEquivalents -dict-add "Quit Plexamp" "@q"
  '';

  # Home Manager configuration
  home-manager = {
    useGlobalPkgs = true;
    useUserPackages = true;
    extraSpecialArgs = {
      inherit inputs;
    };
    users = {
      ben.home = {
        username = "ben";
        homeDirectory = "/Users/ben";
        stateVersion = "23.11";

        sessionPath = [
          "/opt/homebrew/bin"
        ];

        file = {
          ".config/karabiner/karabiner.json".source = ./files/karabiner.json;
          ".config/aerospace/aerospace.toml".source = ./files/aerospace.toml;
        };
      };
    };
  };

  system.stateVersion = 5;
}
