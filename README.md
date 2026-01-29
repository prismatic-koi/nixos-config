# My NixOS configuration

Does anybody even read the readme for nix repos? the code speaks for itself.

## Highlights
- Multiple Nix configurations
    - NixOS desktop & test VM
- Both Nix, and Home Manager configurations rolled into a single set up.
    - I've recently taken this a step further, combining configurations in the same files
    This makes this setup much simpler and cleaner but is only now suitable for a single user setup
- [Impermanence](https://github.com/nix-community/impermanence)
- Initial disk formatting and setup (btrfs) with [disko](https://github.com/nix-community/disko)
- Secret encryption and management with [sops-nix](https://github.com/Mic92/sops-nix)
- Extensively configured wayland environments (sway and hyprland) and editor (neovim)
- A homespun theming system usually using [everforest](https://github.com/sainnhe/everforest)
- A dynamic wallpaper based on theme, created from a svg with theme colours inserted at build time.
- Due to the above, I am able to do remote installs using [nixos-anywhere](https://github.com/nix-community/nixos-anywhere)

My [nix-darwin](https://github.com/LnL7/nix-darwin) configuration has moved to [nixdarwin-config](https://github.com/prismatic-koi/nixdarwin-config)
