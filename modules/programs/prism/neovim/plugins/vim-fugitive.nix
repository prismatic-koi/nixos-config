{
  config,
  lib,
  pkgs,
  ...
}:
{
  home-manager.users.ben.programs.neovim.plugins = [
    pkgs.vimPlugins.vim-rhubarb
    {
      plugin = pkgs.vimPlugins.vim-fugitive;
      type = "lua";
      config =
        # lua
        ''
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
          			if vim.api.nvim_buf_get_option(bufnr, "modified") then
          				vim.notify("You have unsaved buffers!", vim.log.levels.WARN, { title = "Fugitive" })
          			end
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
