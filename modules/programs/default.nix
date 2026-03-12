{
  lib,
  config,
  pkgs,
  isLinux,
  ...
}:
{
  imports = [
    ./anki.nix
    ./aws.nix
    ./bitwarden.nix
    ./calibre.nix
    ./choose.nix
    ./chromium.nix
    ./claude-code.nix
    ./cura-appimage.nix
    ./darktable.nix
    ./dragon-drop.nix
    ./fastfetch.nix
    ./firefox
    ./gcalcli.nix
    ./gimp.nix
    ./git.nix
    ./gitlab-cli.nix
    ./golang.nix
    ./home-automation.nix
    ./k9s.nix
    ./kitty.nix
    ./kubetools.nix
    ./lf.nix
    ./libreoffice.nix
    ./mpv.nix
    ./ncmpcpp.nix
    ./nh.nix
    ./obsidian.nix
    ./picard.nix
    ./plexamp.nix
    ./podman.nix
    ./prism
    ./python.nix
    ./qutebrowser
    ./rclone.nix
    ./rmpc.nix
    ./signal.nix
    ./ssh.nix
    ./vimiv.nix
    ./virt-manager.nix
    ./voice-to-text-daemon.nix
    ./waydroid.nix
    ./wgnord.nix
    ./discord.nix
    ./zathura.nix
    ./zsh.nix
  ]
  ++ lib.optionals isLinux [
    ./corectrl.nix
    ./mangohud.nix
  ];
  options = {
    nx.programs = {
      # Define the available browsers as an enum option
      defaultWebBrowser = lib.mkOption {
        default = "qutebrowser";
        type = lib.types.enum [
          "firefox"
          "qutebrowser"
        ];
        description = "Default web browser to use.";
      };

      # Generate a structured object based on the selected browser
      defaultWebBrowserSettings = lib.mkOption {
        default = {
          name = "qutebrowser";
          cmd = "qutebrowser";
          newWindowCmd = "qutebrowser --target window";
        };
        type = lib.types.attrs;
        internal = true; # This is generated dynamically, not set directly
      };
    };
  };
  config =
    let
      browserSettings = {
        firefox = {
          name = "firefox";
          cmd = "firefox";
          newWindowCmd = "firefox --new-window";
        };
        qutebrowser = {
          name = "qutebrowser";
          cmd = "qutebrowser";
          newWindowCmd = "qutebrowser --target window";
        };
      };
    in
    {
      nx.programs.defaultWebBrowserSettings = lib.mkDefault (
        lib.attrByPath [ config.nx.programs.defaultWebBrowser ] { } browserSettings
      );
      home-manager.users.${config.nx.username} = {
        # xdg.mimeApps is Linux-only
        xdg.mimeApps.enable = pkgs.stdenv.isLinux;
        # some default programs that require no configuration
        home.packages =
          with pkgs;
          [
            age
            cachix
            curl
            dig
            direnv
            dust
            eza
            fdupes
            file
            fzf
            fzy
            google-cloud-sdk
            htop
            imagemagick
            jnv
            jq
            killall
            nixfmt
            openssl
            p7zip
            pdftk
            ripgrep
            socat
            sops
            sqlite
            unrar
            unzip
            wget
            xdg-utils
            yq-go
          ]
          ++ lib.optionals pkgs.stdenv.isLinux [
            yt-dlp # Linux-only due to D-Bus dependencies
          ];
      };
    };
}
