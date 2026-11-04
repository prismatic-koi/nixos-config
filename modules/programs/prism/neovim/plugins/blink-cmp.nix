{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.blink-cmp;
      type = "lua";
      config =
        # lua
        ''
          package.preload["obsidian-frontmatter"] = function()
          ${builtins.readFile ./obsidian-frontmatter-source.lua}
          end
          local function prose_min_length(ctx)
          	local ft = vim.bo[ctx.bufnr].filetype
          	if ft == "markdown" or ft == "text" or ft == "gitcommit" then
          		return 2
          	end
          	return 0
          end
          vim.api.nvim_create_autocmd("User", {
          	pattern = "ObsidianNoteEnter",
          	callback = function(ev)
          		local buf = ev.buf
          		vim.schedule(function()
          			if vim.api.nvim_buf_is_valid(buf) then
          				require("obsidian-frontmatter").prewarm(buf)
          			end
          		end)
          	end,
          })
          require("blink.cmp").setup({
          	keymap = {
          		preset = "none",
          		["<C-n>"] = { "select_next", "fallback" },
          		["<C-p>"] = { "select_prev", "fallback" },
          		["<C-d>"] = { "scroll_documentation_up", "fallback" },
          		["<C-f>"] = { "scroll_documentation_down", "fallback" },
          		["<C-s>"] = { "show", "fallback" },
          		["<CR>"] = { "select_and_accept", "fallback" },
          		["<Tab>"] = { "select_next", "snippet_forward", "fallback" },
          		["<S-Tab>"] = { "select_prev", "snippet_backward", "fallback" },
          	},
          	completion = {
          		trigger = {
          			show_on_blocked_trigger_characters = function()
          				if vim.bo.filetype == "markdown" then
          					return { "\n", "\t" }
          				end
          				return { " ", "\n", "\t" }
          			end,
          		},
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
          		default = { "lsp", "snippets", "buffer", "path", "obsidian_frontmatter" },
          		min_keyword_length = 0,
          		providers = {
          			lsp = {
          				min_keyword_length = prose_min_length,
          				-- obsidian.nvim offers frontmatter tags (`Tag: x`); the custom source owns those.
          				transform_items = function(ctx, items)
          					if vim.bo[ctx.bufnr].filetype ~= "markdown" then
          						return items
          					end
          					return vim.tbl_filter(function(item)
          						local label = item.label
          						return not (type(label) == "string" and label:match("^Tag: ") and not label:match("^Tag: #"))
          					end, items)
          				end,
          			},
          			snippets = { min_keyword_length = prose_min_length },
          			path = { min_keyword_length = prose_min_length },
          			buffer = { min_keyword_length = 5 },
          			obsidian_frontmatter = {
          				name = "ObsidianFrontmatter",
          				module = "obsidian-frontmatter",
          				min_keyword_length = 0,
          			},
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
