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
            -- obsidian.nvim draws checkboxes and bullets through its own ui block.
            -- Wikilinks are the one link part that is on here. The pipe table sizes a cell
            -- from its own marks only, so a wikilink that obsidian.nvim concealed broke the row.
            -- obsidian.nvim skips wikilinks in its ui (see obsidian.nix).
            -- The inline link renderer is switched off below, so other links stay as before.
            require("render-markdown.render.inline.link").setup = function()
            	return false
            end
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
            	link = {
            		enabled = true,
            		footnote = { enabled = false },
            		wiki = {
            			enabled = true,
            			-- No icon, so the text matches the old obsidian.nvim look.
            			icon = "",
            			scope_highlight = "ObsidianRefText",
            		},
            	},
            	paragraph = { enabled = false },
            	sign = { enabled = false },
            	yaml = { enabled = false },
            	-- The plugin default is 3, which hides the replacement character of a concealed
            	-- extmark. obsidian.nvim checkboxes and bullets need level 2 to show their symbol.
            	win_options = { conceallevel = { rendered = 2 } },
            })
          '';
      }
    ];
  };
}
