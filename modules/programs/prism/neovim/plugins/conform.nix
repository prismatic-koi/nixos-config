{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username} = {
    home.packages = with pkgs; [
      stylua
      nixfmt
      nixfmt-tree
      black
      jq
    ];
    programs.neovim.plugins = [
      {
        plugin = pkgs.vimPlugins.conform-nvim;
        type = "lua";
        config =
          # lua
          ''
            require("conform").setup({
            	formatters_by_ft = {
            		lua = { "stylua" },
            		python = { "black" },
            		nix = { "nixfmt", "injected" },
            		json = { "jq" },
            	},
            	formatters = {
            		nixfmt = {
            			command = "${pkgs.nixfmt}/bin/nixfmt",
            		},
            	},
            	format_on_save = {
            		lsp_format = "fallback",
            	},
            })
            -- Keybindings
            vim.keymap.set("n", "<leader>f", function()
            	require("conform").format({ async = true, lsp_format = "fallback" })
            end, { desc = "Format the current buffer" })
          '';
      }
    ];
  };
}
