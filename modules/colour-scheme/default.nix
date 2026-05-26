{
  pkgs,
  lib,
  ...
}:
{
  imports = [
    ./schema.nix
    ./catppuccin-latte.nix
    ./edge.nix
    ./everforest.nix
    ./github-light.nix
    ./gruvbox.nix
    ./nightcity-kabuki.nix
    ./onedark.nix
  ];
  options = {
    nx.desktop.theme = lib.mkOption {
      default = "everforest";
      type = lib.types.enum [
        "catppuccin-latte"
        "edge"
        "everforest"
        "github-light"
        "gruvbox-dark"
        "gruvbox-light"
        "nightcity-kabuki"
        "onedark"
      ];
    };
  };
}
