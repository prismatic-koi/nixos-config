{
  pkgs,
  inputs,
  config,
  lib,
  ...
}:
let
  username = config.nx.username;
in
{
  imports = [
    ../../modules
  ];

  nx.username = "bensherman";

  users.users.${username} = {
    home = "/Users/${username}";
  };

  networking.hostName = "m4mac";

  # Module configuration using nx namespace (matching NixOS pattern)
  nx = {
    programs = {
      prism.opencode.provider = "google";
      # Disable programs that default to enabled but aren't needed/available on Darwin
      podman.enable = false;
      dragon-drop.enable = false;
      nh.enable = false;
      chromium.enable = false; # Not available on Darwin
      bitwarden.enable = false; # Use Homebrew cask instead
      discord.enable = false; # Not commonly used on Darwin
      gimp.enable = false; # Not commonly used on Darwin
      signal.enable = false; # Not available on Darwin
      vimiv.enable = false; # Linux-only (Qt GPU dependencies)
      mpv.enable = false;
      zathura.enable = false;
      ssh.enableWorkKeys = true;
      homeAutomation.enable = true;
      aws.enable = true;
      gitlab-cli.enable = true;
    };
    desktop = {
      theme = "onedark";
      rofi.enable = true; # For shopping list script (uses choose on Darwin)
    };
    services = {
      flakeUpdateNotifier.enable = true;
      syncthing = {
        enable = true;
        obsidian.enable = true;
      };
    };
  };

  # Darwin-specific packages not in shared modules
  environment.systemPackages = with pkgs; [
    arping
    gnutar
    mysql80
    podman # darwin doesn't use virtualisation.podman
    postgresql
    rustup
    tree
    tridactyl-native
    utm
  ];

  security.sudo.extraConfig = ''
    ${username} ALL=(ALL:ALL) NOPASSWD: ALL
  '';

  services = {
    karabiner-elements.enable = false;
  };

  system.primaryUser = username;

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
      "raycast"
      "scroll-reverser"
    ];
  };

  # Activation scripts
  system.activationScripts.extraActivation.text = ''
    # Set Cmd+Q shortcut for Plexamp (Electron app workaround)
    # Run as user since defaults needs to write to user preferences
    sudo -u ${username} /usr/bin/defaults write tv.plex.plexamp NSUserKeyEquivalents -dict-add "Quit Plexamp" "@q"
  '';

  # Home Manager configuration
  home-manager = {
    useGlobalPkgs = true;
    useUserPackages = true;
    extraSpecialArgs = {
      inherit inputs;
    };
    users.${username} = {
      programs.chromium = {
        enable = false;
        package = pkgs.hello; # override Linux-only default so Darwin eval doesn't fail
      };
      # launchd user agent to supervise borders (jankyborders) with automatic restart
      launchd.agents.jankyborders = {
        enable = true;
        config = {
          ProgramArguments = [
            "${pkgs.jankyborders}/bin/borders"
            "active_color=0xffa7c080"
            "inactive_color=0x00232a2e"
            "width=10"
          ];
          KeepAlive = true;
          RunAtLoad = true;
          StandardOutPath = "/tmp/jankyborders.log";
          StandardErrorPath = "/tmp/jankyborders.log";
        };
      };
      home = {
        username = username;
        homeDirectory = "/Users/${username}";
        stateVersion = "25.11";

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
