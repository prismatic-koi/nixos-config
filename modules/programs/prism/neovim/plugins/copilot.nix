{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username} = {
    programs.neovim.plugins = [
      {
        plugin = pkgs.vimPlugins.copilot-lua;
        type = "lua";
        config =
          # lua
          ''
            require("copilot").setup({
            	copilot_node_command = "${pkgs.nodejs_24}/bin/node",
            	panel = {
            		enabled = false,
            	},
            	suggestion = {
            		enabled = true,
            		auto_trigger = true,
            		hide_during_completion = true,
            		debounce = 15,
            		keymap = {
            			accept = "<C-L>",
            			next = "<C-J>",
            			prev = "<C-K>",
            		},
            	},
            	filetypes = {
            		markdown = true,
            		yaml = true,
            		help = false,
            		gitcommit = false,
            		gitrebase = false,
            		hgcommit = false,
            		svn = false,
            		cvs = false,
            		["."] = true,
            		rust = false, -- while learning rust, no copilot
            	},
            })

            -- copilot-lua's suggestion.hide_during_completion only understands
            -- nvim-cmp's events natively. Since the completion engine migrated
            -- to blink.cmp, re-wire the same ghost-text suppression to blink's
            -- BlinkCmpMenuOpen / BlinkCmpMenuClose User autocmd events.
            vim.api.nvim_create_autocmd("User", {
            	pattern = "BlinkCmpMenuOpen",
            	callback = function()
            		vim.b.copilot_suggestion_hidden = true
            	end,
            })
            vim.api.nvim_create_autocmd("User", {
            	pattern = "BlinkCmpMenuClose",
            	callback = function()
            		vim.b.copilot_suggestion_hidden = false
            	end,
            })
          '';
      }
    ];
    home.persistence."/persist" = {
      directories = [
        ".config/github-copilot"
      ];
    };
  };
}
