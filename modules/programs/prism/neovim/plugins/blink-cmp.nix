{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.blink-cmp;
      type = "lua";
      config =
        # lua
        ''
          require("blink.cmp").setup({
          	keymap = {
          		preset = "none",
          		["<C-n>"] = { "select_next", "fallback" },
          		["<C-p>"] = { "select_prev", "fallback" },
          		["<C-d>"] = { "scroll_documentation_up", "fallback" },
          		["<C-f>"] = { "scroll_documentation_down", "fallback" },
          		["<C-Space>"] = { "show", "fallback" },
          		["<CR>"] = { "select_and_accept", "fallback" },
          		["<Tab>"] = { "select_next", "snippet_forward", "fallback" },
          		["<S-Tab>"] = { "select_prev", "snippet_backward", "fallback" },
          	},
          	completion = {
          		accept = {
          			auto_brackets = { enabled = false },
          		},
          	},
          	fuzzy = {
          		max_typos = function(keyword)
          			local ft = vim.bo.filetype
          			if ft == "markdown" or ft == "text" or ft == "gitcommit" then
          				return 0
          			end
          			return math.floor(#keyword / 4)
          		end,
          	},
          	snippets = { preset = "luasnip" },
          	sources = {
          		default = { "lsp", "snippets", "buffer", "path" },
          		min_keyword_length = function(ctx)
          			local ft = vim.bo[ctx.bufnr].filetype
          			if ft == "markdown" or ft == "text" or ft == "gitcommit" then
          				return 2
          			end
          			return 0
          		end,
          		providers = {
          			buffer = { min_keyword_length = 5 },
          		},
          	},
          	cmdline = {
          		keymap = { preset = "cmdline" },
          		sources = { "buffer", "cmdline" },
          		completion = {
          			menu = { auto_show = true },
          		},
          	},
          })
        '';
    }
  ];
}
