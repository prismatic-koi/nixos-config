{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username} = {
    programs.neovim.plugins = [
      {
        plugin = pkgs.vimPlugins.render-markdown-nvim;
        type = "lua";
        config =
          # lua
          ''
            -- Only pipe tables, block quotes, and callouts are on. Every other part is off
            -- by name, so a plugin update that adds a default cannot turn it back on.
            -- obsidian.nvim draws checkboxes, bullets, and links through its own ui block.
            require("render-markdown").setup({
            	enabled = true,
            	completions = {
            		blink = { enabled = false },
            		coq = { enabled = false },
            		lsp = { enabled = false },
            	},
            	pipe_table = {
            		enabled = true,
            		preset = "none",
            		style = "full",
            		cell = "padded",
            		head = "RenderMarkdownTableRow",
            	},
            	quote = { enabled = true },
            	anti_conceal = { enabled = true },
            	bullet = { enabled = false },
            	checkbox = { enabled = false },
            	code = { enabled = false, sign = false },
            	dash = { enabled = false },
            	document = { enabled = false },
            	heading = { enabled = false, sign = false },
            	html = { enabled = false },
            	indent = { enabled = false },
            	inline_highlight = { enabled = false },
            	latex = { enabled = false },
            	link = { enabled = false },
            	paragraph = { enabled = false },
            	sign = { enabled = false },
            	yaml = { enabled = false },
            })
          '';
      }
    ];
  };
}
