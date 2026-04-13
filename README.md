# My NixOS configuration

Does anybody even read the readme for nix repos? The code speaks for itself.

## Highlights
- Multiple Nix configurations
    - NixOS desktop
    - NixDarwin
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

## Smoke-testing the prism-agent container image

The `prism-agent` container image can be verified with a smoke test that checks all required tools are present and CA certificates work correctly.

```bash
# Build the image
nix build .#prismAgentImage

# Load it into podman (on Darwin, start the VM first: podman machine start)
podman load < result

# Run the smoke test (defaults to localhost/prism-agent:latest)
./modules/programs/prism/prism/scripts/smoke-test-container.sh

# Or test a specific image
./modules/programs/prism/prism/scripts/smoke-test-container.sh localhost/prism-agent:latest
```

The script exits `0` if all checks pass and non-zero if any fail. It skips gracefully if `podman` is not in PATH.
