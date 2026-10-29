{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username} = {
    home.packages = [
      pkgs.fzf
    ];
    programs.neovim.plugins = [
      {
        plugin = pkgs.vimPlugins.telescope-fzf-native-nvim;
        type = "lua";
        config = "";
      }
      {
        plugin = pkgs.vimPlugins.telescope-nvim;
        type = "lua";
        config =
          # lua
          ''
            local square_borders = { "─", "│", "─", "│", "┌", "┐", "┘", "└" }

            -- Each git command of the git pickers and previewers gets these
            -- options after its subcommand. A planted diff.external, a textconv
            -- driver, or a submodule with ignore = none runs no command (#3347).
            -- The pickers and previewers copy utils.__git_command when they
            -- load, so the wrap must come first.
            local git_flags = {
            	diff = { "--no-ext-diff", "--no-textconv", "--ignore-submodules=all" },
            	log = { "--no-ext-diff", "--no-textconv" },
            	show = { "--no-ext-diff", "--no-textconv" },
            	status = { "--ignore-submodules=all" },
            }
            local telescope_utils = require("telescope.utils")
            local git_command = telescope_utils.__git_command
            local git_hardened = type(git_command) == "function"
            	and not package.loaded["telescope.builtin.__git"]
            	and not package.loaded["telescope.previewers.buffer_previewer"]
            if git_hardened then
            	telescope_utils.__git_command = function(args, opts)
            		local out, i = {}, 1
            		while args[i] and args[i]:sub(1, 1) == "-" do
            			if args[i] == "-c" or args[i] == "-C" then
            				table.insert(out, args[i])
            				i = i + 1
            			end
            			table.insert(out, args[i])
            			i = i + 1
            		end
            		local subcommand = args[i]
            		if subcommand == "stash" and args[i + 1] == "show" then
            			table.insert(out, subcommand)
            			i = i + 1
            			subcommand = "show"
            		end
            		table.insert(out, args[i])
            		vim.list_extend(out, git_flags[subcommand] or {})
            		vim.list_extend(out, args, i + 1)
            		return git_command(out, opts)
            	end
            end

            require("telescope").setup({
            	defaults = {
            		mappings = {
            			i = {
            				["<C-u>"] = false,
            				["<C-d>"] = false,
            			},
            		},
            		borderchars = {
            			square_borders,
            			prompt = square_borders,
            			results = square_borders,
            			preview = square_borders,
            		},
            	},
            	pickers = {
            		live_grep = {
            			additional_args = { "--hidden" },
            		},
            		find_files = {
            			hidden = true,
            		},
            	},
            })
            -- Enable telescope fzf native, if installed
            pcall(require("telescope").load_extension, "fzf")
            -- A git picker runs git in the repo of the nvim cwd, which can be a
            -- nested repo. Give it the git dir and the work tree of the startup
            -- dir on the git command line, so git does no discovery.
            local git_root = require("prism.git_root")
            local telescope_builtin = require("telescope.builtin")
            for _, name in ipairs({
            	"git_files",
            	"git_commits",
            	"git_bcommits",
            	"git_bcommits_range",
            	"git_branches",
            	"git_status",
            	"git_stash",
            }) do
            	local picker = telescope_builtin[name]
            	telescope_builtin[name] = function(opts)
            		if not git_hardened then
            			vim.notify(
            				"telescope " .. name .. ": refused. The wrap of telescope.utils.__git_command did not load.",
            				vim.log.levels.ERROR
            			)
            			return
            		end
            		if not git_root.root then
            			vim.notify("telescope " .. name .. ": refused. nvim did not start in a git repo.", vim.log.levels.WARN)
            			return
            		end
            		return picker(vim.tbl_extend("force", opts or {}, {
            			cwd = git_root.root,
            			toplevel = git_root.root,
            			gitdir = git_root.gitdir,
            		}))
            	end
            end
            -- Keymaps
            vim.keymap.set("n", "<leader>sr", ":Telescope oldfiles<CR>", { desc = "[S]earch [R]ecently opened files" })
            vim.keymap.set("n", "<leader>sf", ":Telescope find_files<CR>", { desc = "[S]earch [F]iles" })
            vim.keymap.set("n", "<leader>st", ":Telescope git_files<CR>", { desc = "[S]earch [T]racked files (git)" })
            vim.keymap.set("n", "<leader>sc", ":Telescope git_commits<CR>", { desc = "[S]earch [C]ommits (git)" })
            vim.keymap.set("n", "<leader>sh", ":Telescope help_tags<CR>", { desc = "[S]earch [H]elp" })
            vim.keymap.set("n", "<leader>sw", ":Telescope grep_string<CR>", { desc = "[S]earch [W]ord" })
            vim.keymap.set("n", "<leader>sg", ":Telescope live_grep<CR>", { desc = "[S]earch [G]rep" })
            vim.keymap.set("n", "<leader>sd", ":Telescope diagnostics<CR>", { desc = "[S]earch [D]iagnostics" })
            vim.keymap.set("n", "<leader>sk", ":Telescope keymaps<CR>", { desc = "[S]earch [K]eymaps" })
            vim.keymap.set("n", "<leader>sb", ":Telescope buffers<CR>", { desc = "[S]earch [B]uffers" })
            vim.keymap.set("n", "<leader>sj", ":Telescope jumplist<CR>", { desc = "[S]earch [J]umplist" })
          '';
      }
    ];
  };
}
