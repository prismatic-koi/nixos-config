{ config, pkgs, ... }:
{
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

            -- image.nvim opens the tmux output as a tty path without a shell_error check.
            -- A dead tmux server then creates a stray file. See 3rd/image.nvim lua/image/utils/tmux.lua.
            local image_tmux = require("image/utils/tmux")
            local get_pane_tty = image_tmux.get_pane_tty
            image_tmux.get_pane_tty = function()
            	local tty = get_pane_tty()
            	if vim.v.shell_error ~= 0 or type(tty) ~= "string" or tty:sub(1, 5) ~= "/dev/" then
            		return nil
            	end
            	return tty
            end
          '';
      }
    ];
  };
}
