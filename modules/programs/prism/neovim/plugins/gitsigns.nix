{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.gitsigns-nvim;
      type = "lua";
      config =
        # lua
        ''
          local git_root = require("prism.git_root")
          require("gitsigns").setup({
          	-- The automatic attach runs git in the dir of the buffer. That git
          	-- uses a nested .git or a submodule that a worker wrote (#3347).
          	-- The autocmd below attaches to the repo of the startup dir.
          	auto_attach = false,
          	signcolumn = true,
          	current_line_blame = true,
          	current_line_blame_opts = {
          		virt_text = true,
          		virt_text_pos = "eol",
          		delay = 200,
          		ignore_whitespace = false,
          		-- Current-line blame runs `git blame --contents` on the working
          		-- buffer. Without this, a planted diff textconv driver that a
          		-- .gitattributes selects runs on the host. --no-textconv stops
          		-- it. A planted clean filter still runs when git normalises the
          		-- buffer; that needs a planted config and is a known limit
          		-- (#3346). See user-git-overrides.md.
          		extra_opts = { "--no-textconv" },
          	},
          })
          -- The attach gives the git dir and the work tree on the git command
          -- line, so git does no discovery. A file in a nested repo is not in
          -- the index of the worktree, so gitsigns does not attach to it.
          vim.api.nvim_create_autocmd({ "BufFilePost", "BufRead", "BufNewFile", "BufWritePost" }, {
          	group = vim.api.nvim_create_augroup("prismatic-koi_gitsigns_attach", { clear = true }),
          	callback = function(args)
          		local bufnr = args.buf
          		local name = vim.api.nvim_buf_get_name(bufnr)
          		if not git_root.root or vim.bo[bufnr].buftype ~= "" or name == "" then
          			return
          		end
          		local path = vim.uv.fs_realpath(name) or name
          		local stat = vim.uv.fs_stat(path)
          		if (stat and stat.type == "directory") or not vim.uv.fs_stat(vim.fs.dirname(path)) then
          			return
          		end
          		if vim.api.nvim_buf_line_count(bufnr) > require("gitsigns.config").config.max_file_length then
          			return
          		end
          		require("gitsigns").attach(
          			bufnr,
          			{ file = path, toplevel = git_root.root, gitdir = git_root.gitdir },
          			args.event
          		)
          	end,
          })
          vim.keymap.set("n", "<leader>gph", require("gitsigns").prev_hunk, { buffer = bufnr, desc = "[G]it [P]revious [H]unk" })
          vim.keymap.set("n", "<leader>gnh", require("gitsigns").next_hunk, { buffer = bufnr, desc = "[G]it [N]ext [H]unk" })
          vim.keymap.set("n", "<leader>gsh", require("gitsigns").stage_hunk, { buffer = bufnr, desc = "[G]it [S]tage [H]unk" })
          vim.keymap.set(
          	"n",
          	"<leader>guh",
          	require("gitsigns").undo_stage_hunk,
          	{ buffer = bufnr, desc = "[G]it [U]nstage [H]unk" }
          )

          vim.keymap.set(
          	"n",
          	"<leader>gtd",
          	require("gitsigns").toggle_deleted,
          	{ buffer = bufnr, desc = "[G]it [T]oggle [D]eleted" }
          )
          vim.keymap.set(
          	"n",
          	"<leader>gtb",
          	require("gitsigns").toggle_current_line_blame,
          	{ buffer = bufnr, desc = "[G]it [T]oggle [B]lame" }
          )
        '';
    }
  ];
}
