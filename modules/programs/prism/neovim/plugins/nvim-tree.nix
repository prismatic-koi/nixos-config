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
          local git_root = require("prism.git_root")
          -- The tree runs `git status` at the root of each repo that it shows.
          -- --ignore-submodules=all stops a `git status` in a submodule that a
          -- worker staged with ignore = none in .gitmodules (#3347). The tree
          -- has no option for git arguments, so this wraps a private method.
          -- If the method changes, the tree turns off its git integration.
          local git_runner_ok, git_runner = pcall(require, "nvim-tree.git.runner")
          local get_spawn_options = git_runner_ok and type(git_runner) == "table" and git_runner.get_spawn_options
          local function harden_spawn_options(self, ...)
          	local options = get_spawn_options(self, ...)
          	local args = {}
          	for i = 1, table.maxn(options.args) do
          		if options.args[i] ~= nil then
          			table.insert(args, options.args[i])
          			if options.args[i] == "status" then
          				table.insert(args, "--ignore-submodules=all")
          			end
          		end
          	end
          	options.args = args
          	return options
          end
          local git_hardened = type(get_spawn_options) == "function"
          	and pcall(function()
          		local probe = harden_spawn_options({ toplevel = "/", path = "/", list_untracked = true, list_ignored = true })
          		assert(vim.tbl_contains(probe.args, "--ignore-submodules=all"))
          	end)
          if git_hardened then
          	git_runner.get_spawn_options = harden_spawn_options
          else
          	vim.notify("nvim-tree: git integration off. The git runner did not take --ignore-submodules.", vim.log.levels.ERROR)
          end
          require("nvim-tree").setup({
          	filters = {
          		--disable the filtering out of dotfiles
          		dotfiles = false,
          	},
          	git = {
          		enable = git_hardened,
          		ignore = false,
          		timeout = 500,
          		-- A nested repo or a submodule has its own toplevel, and a worker
          		-- writes its git dir (#3347). Run git only in the startup repo.
          		disable_for_dirs = function(toplevel)
          			return not git_root.is_root(toplevel)
          		end,
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
