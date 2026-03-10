{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    pkgs.vimPlugins.nvim-web-devicons
    {
      plugin = pkgs.vimPlugins.oil-nvim;
      type = "lua";
      config =
        # lua
        ''
          require("oil").setup({
          	view_options = {
          		show_hidden = true,
          	},
          	default_file_explorer = true,
          })
          vim.keymap.set("n", "-", vim.cmd.Oil, { desc = "open parent directory" })
        '';
    }
  ];
}
