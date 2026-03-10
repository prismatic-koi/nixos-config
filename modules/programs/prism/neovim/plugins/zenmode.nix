{
  config,
  lib,
  pkgs,
  ...
}:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.zen-mode-nvim;
      type = "lua";
      config =
        # lua
        ''
          require("zen-mode").setup({
            window = {
              backdrop = 1,
              width = 1,
              options = {
                number = false,
              },
            },
          	plugins = {
              gitsigns = { enabled = false },
          		tmux = { enabled = true },
          		kitty = {
          			enabled = true,
          			font = "+2",
          		},
          	},
            on_open = function(win)
              local gs = require("gitsigns")
              local config = require("gitsigns.config").config
              config.current_line_blame = false
              config.current_line_blame_opts.virt_text = false
              gs.refresh()
            end,
            on_close = function(win)
              local gs = require("gitsigns")
              local config = require("gitsigns.config").config
              config.current_line_blame = true
              config.current_line_blame_opts.virt_text = true
              gs.refresh()
            end,
          })
          vim.keymap.set("n", "<leader>z", vim.cmd.ZenMode, { desc = "[Z]enMode" })
        '';
    }
  ];
}
