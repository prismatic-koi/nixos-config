{ config, ... }:
{
  # The git plugins of nvim run git only in the repo of the startup dir. A
  # sandboxed worker can write a .git dir, a .git file, or a submodule in a
  # subdir of its worktree. git that finds that git dir runs its drivers on
  # the host (#3347). See prism/docs/invariants/host-git-commands.md.
  home-manager.users.${config.nx.username}.programs.neovim.initLua =
    # lua
    ''
      package.loaded["prism.git_root"] = (function()
      	local uv = vim.uv
      	local M = {}

      	local function gitdir_of(root)
      		local dotgit = root .. "/.git"
      		local stat = uv.fs_stat(dotgit)
      		if stat and stat.type == "directory" then
      			return uv.fs_realpath(dotgit)
      		end
      		if stat and stat.type == "file" then
      			local file = io.open(dotgit, "r")
      			local line = file and file:read("*l")
      			if file then
      				file:close()
      			end
      			local path = line and line:match("^gitdir:%s*(.-)%s*$")
      			if path and path ~= "" then
      				if path:sub(1, 1) ~= "/" then
      					path = root .. "/" .. path
      				end
      				return uv.fs_realpath(path)
      			end
      		end
      	end

      	-- The top of the worktree, as git discovery finds it: the nearest dir
      	-- at or above the startup dir that holds a .git entry.
      	local dir = uv.fs_realpath(uv.cwd() or "")
      	while dir and not uv.fs_lstat(dir .. "/.git") do
      		local parent = vim.fs.dirname(dir)
      		dir = parent ~= dir and parent or nil
      	end
      	if dir then
      		M.gitdir = gitdir_of(dir)
      		M.root = M.gitdir and dir or nil
      	end

      	function M.is_root(path)
      		return M.root ~= nil and path ~= nil and uv.fs_realpath(path) == M.root
      	end

      	return M
      end)()
    '';
}
