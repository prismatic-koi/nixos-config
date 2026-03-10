{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    pkgs.vimPlugins.nvim-web-devicons
    {
      plugin = pkgs.vimPlugins.nvim-tree-lua;
      type = "lua";
      config =
        # lua
        ''
          require("nvim-tree").setup({
          	filters = {
          		--disable the filtering out of dotfiles
          		dotfiles = false,
          	},
          	git = {
          		enable = true,
          		ignore = false,
          		timeout = 500,
          	},
          	update_focused_file = {
          		enable = true,
          		update_root = true,
          		ignore_list = {},
          	},
          	view = {
          		width = 50,
          	},
          })
          vim.keymap.set("n", "<leader>et", vim.cmd.NvimTreeToggle, { desc = "[E]xplorer [T]oggle" })
        '';
    }
  ];
}
