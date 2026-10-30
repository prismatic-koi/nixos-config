{
  config,
  lib,
  pkgs,
  ...
}:
let
  submoduleGuard = config.nx.programs.git.submoduleGuard;
in
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    pkgs.vimPlugins.vim-rhubarb
    {
      plugin = pkgs.vimPlugins.vim-fugitive;
      type = "lua";
      config =
        # lua
        ''
            -- fugitive reads g:fugitive_git_executable to locate git. A worker
            -- can stage a submodule and set ignore = none in .gitmodules,
            -- which overrides diff.ignoreSubmodules = all; a typed :Git status
            -- then runs git in the submodule git dir and runs a planted filter
            -- (#3343). The guard adds --ignore-submodules=all to the status
            -- and diff commands fugitive runs, so :Git runs no such command.
            -- Set it before any :Git runs.
            vim.g.fugitive_git_executable = { "${submoduleGuard}" }

            -- autocommand to set keybindings only in fugitive buffers
            local augroup = vim.api.nvim_create_augroup("prismatic-koi_fugitive", {})
            vim.api.nvim_create_autocmd("FileType", {
            	group = augroup,
            	pattern = "*",
            	callback = function()
            		-- only applies to files with filetype fugitive
            		if vim.bo.ft ~= "fugitive" then
            			return
            		end
          local bufnr = vim.api.nvim_get_current_buf()

          		-- unmap fugitive's default 'p' (patch command, unused)
          		pcall(vim.keymap.del, "n", "p", { buffer = bufnr })

          		-- p<space> populates command line with :!prism quick  ready to type e.g. pr<CR>
          		vim.keymap.set("n", "p", function()
          			vim.api.nvim_feedkeys(":!prism quick ", "n", false)
          		end, { buffer = bufnr, desc = "[P]rism quick command" })

          		-- keymap for git push
            		vim.keymap.set("n", "<leader>p", function()
            			vim.cmd.Git("push")
            		end, { buffer = bufnr, desc = "[P]ush" })

            		-- keymap for git fetch
            		vim.keymap.set("n", "<leader>f", function()
            			vim.cmd.Git("fetch")
            		end, { buffer = bufnr, desc = "[F]etch" })

            		-- keymap for git pull with rebase
            		vim.keymap.set("n", "<leader>P", function()
                  vim.cmd.Git('pull --rebase --autostash')
            		end, { buffer = bufnr, desc = "[P]ull with rebase" })

            		-- open git repo in browser (requires vim-rhubarb)
            		vim.keymap.set("n", "<leader>gb", ":GBrowse<CR>", { desc = "[G]it [B]rowse" })

            		-- quit buffer
            		vim.keymap.set("n", "q", function()
            			vim.cmd("q")
            		end, { buffer = bufnr, desc = "[Q]uit" })
            	end,
            })
            -- warn about unsaved buffers on opening fugitive
            vim.api.nvim_create_autocmd("BufRead", {
            	group = augroup,
            	pattern = "*",
            	callback = function()
            		for _, bufnr in ipairs(vim.api.nvim_list_bufs()) do
            			if vim.bo[bufnr].modified
            				and vim.bo[bufnr].buftype == ""
            				and vim.fn.buflisted(bufnr) == 1 then
            				vim.notify("You have unsaved buffers!", vim.log.levels.WARN, { title = "Fugitive" })
            			end
            		end
            	end,
            })

            -- fugitive finds the git dir of a buffer from the path of the buffer.
            -- For a file in a nested repo or a submodule, that is a git dir that
            -- a worker wrote (#3347). Pin each buffer to the git dir of the
            -- startup dir. fugitive clears b:git_dir on a read and on a rename,
            -- so these autocmds must come after the autocmds of fugitive.
            vim.api.nvim_create_autocmd("VimEnter", {
            	group = augroup,
            	once = true,
            	callback = function()
            		local git_root = require("prism.git_root")
            		local git_dir = git_root.root and vim.fn.FugitiveExtractGitDir(git_root.root) or ""
            		local function pin(bufnr)
            			if not vim.startswith(vim.api.nvim_buf_get_name(bufnr), "fugitive://") then
            				vim.b[bufnr].git_dir = git_dir
            			end
            		end
            		for _, bufnr in ipairs(vim.api.nvim_list_bufs()) do
            			pin(bufnr)
            		end
            		vim.api.nvim_create_autocmd({ "BufNew", "BufReadPost", "BufNewFile", "BufFilePost" }, {
            			group = augroup,
            			callback = function(args)
            				pin(args.buf)
            			end,
            		})
            	end,
            })

            -- Fail closed: a VimEnter probe confirms fugitive routes through
            -- the guard. fugitive#PrepareJob builds the argv for a status
            -- command; its first element must be the guard. If it is not (a
            -- fugitive update drops g:fugitive_git_executable), turn :Git and
            -- <leader>gs into a warning, so an unguarded status cannot run.
            vim.api.nvim_create_autocmd("VimEnter", {
            	group = augroup,
            	once = true,
            	callback = function()
            		local guard = "${submoduleGuard}"
            		local ok, job = pcall(vim.fn["fugitive#PrepareJob"], { "status" })
            		local argv0 = (ok and type(job) == "table" and type(job[1]) == "table") and job[1][1] or nil
            		if argv0 ~= guard then
            			local msg = "fugitive: git is not guarded against a staged submodule; :Git is off (#3343)."
            			vim.api.nvim_create_user_command("Git", function()
            				vim.notify(msg, vim.log.levels.ERROR)
            			end, { bang = true, nargs = "*", range = true })
            			vim.keymap.set("n", "<leader>gs", function()
            				vim.notify(msg, vim.log.levels.ERROR)
            			end, { desc = "[G]it [S]tatus (disabled, #3343)" })
            			vim.notify(msg, vim.log.levels.ERROR)
            		end
            	end,
            })

            vim.keymap.set("n", "<leader>gs", vim.cmd.Git, { desc = "[G]it [S]tatus in Fugitive" })
            vim.keymap.set("n", "<leader>gmd", vim.cmd.Gvdiffsplit, { desc = "[G]it [M]erge [D]iff" })
            vim.keymap.set("n", "<leader>gh", "<cmd>diffget //2<CR>", { desc = "[G]it, left side of merge diff" })
            vim.keymap.set("n", "<leader>gl", "<cmd>diffget //3<CR>", { desc = "[G]it, right side of merge diff" })
        '';
    }
  ];
}
