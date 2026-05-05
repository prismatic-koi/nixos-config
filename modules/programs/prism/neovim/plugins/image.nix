{ config, lib, pkgs, ... }:
# image-nvim uses allow-passthrough to render kitty graphics protocol images.
# On Darwin, allow-passthrough is disabled globally to prevent opencode's
# Bubble Tea TUI from pushing kitty keyboard protocol through to kitty (which
# causes escape keypresses to be swallowed). Disable the plugin on Darwin
# until per-window passthrough control is implemented.
# TODO: re-enable when allow-passthrough is scoped per-window (#allow-passthrough-darwin)
lib.mkIf (!pkgs.stdenv.isDarwin) {
  home-manager.users.${config.nx.username} = {
    programs.neovim.extraLuaPackages = ps: [ ps.magick ];
    programs.neovim.plugins = [
      {
        plugin = pkgs.vimPlugins.image-nvim;
        type = "lua";
        config =
          # lua
          ''
            require("image").setup({
            	backend = "kitty",
            	integrations = {
            		markdown = {
            			enabled = true,
            			clear_in_insert_mode = false,
            			download_remote_images = true,
            			only_render_image_at_cursor = false,
            			filetypes = {
            				"markdown",
            				"vimwiki",
            			},
            		},
            		kitty_method = "normal",
            	},
            })
          '';
      }
    ];
  };
}
