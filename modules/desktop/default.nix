{
  isLinux,
  ...
}:
{
  imports = [
    ./additional-theming.nix
    ./hyprland
    ./quickshell
    ./rofi
    ./screenshot.nix
    ./sway
    ./swaync.nix
    ./wallpaper
    ./waybar
  ]
  ++ (if isLinux then [ ./fonts-linux.nix ] else [ ./fonts-darwin.nix ]);
}
