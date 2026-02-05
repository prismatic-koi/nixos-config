{ ... }:
{
  # Setting various vim options
  home-manager.users.ben.programs.neovim = {
    initLua =
      # lua
      ''
        -- helper
        local map = vim.keymap.set
        -- set leader keys
        vim.g.mapleader = " "
        vim.g.maplocalleader = " "

        -- clipboard
        map("v", "<leader>y", '"+y', { silent = true })
        map("n", "<leader>y", '"+y', { silent = true })
        map("n", "<leader>yy", '"+yy', { silent = true })
        map("n", "<leader>p", '"+p', { silent = true })
        map("v", "<leader>p", '"+p', { silent = true })

        -- Helix inspired redo
        map("n", "U", "<C-r>", { silent = true, desc = "Redo" })
        -- Helix inspired go left and right
        map("n", "gh", "0", { silent = true, desc = "go to start of line" })
        map("n", "gl", "$", { silent = true, desc = "go to end of line" })

        -- quickfix list
        map("n", "<leader>q", ":copen<CR>", { silent = true, desc = "Open quickfix list" })
        map("n", "<leader>j", ":cnext<CR>", { silent = true, desc = "Next quickfix item" })
        map("n", "<leader>k", ":cprev<CR>", { silent = true, desc = "Prev quickfix item" })

        -- move window shortcuts
        map("n", "<C-h>", "<C-w>h", { desc = "Window go left" })
        map("n", "<C-j>", "<C-w>j", { desc = "Window go down" })
        map("n", "<C-k>", "<C-w>k", { desc = "Window go up" })
        map("n", "<C-l>", "<C-w>l", { desc = "Window go right" })

        -- switch to a new session in tmux
        map(
        	"n",
        	"<C-f>",
        	":!tmux neww cli.tmux.projectSessioniser<CR><CR>",
        	{ silent = true, desc = "switch to a new session in tmux" }
        )

        -- Clear search with <esc>
        map({ "i", "n" }, "<esc>", "<cmd>noh<cr><esc>", { desc = "Escape and clear hlsearch" })

        -- make current file executable
        map("n", "<leader>x", ":!chmod +x %<CR>", { silent = true, desc = "make current file executable" })

        -- toggles
        map("n", "<leader>tw", ":set wrap!<CR>", { silent = true, desc = "[T]oggle [W]rap" })
        map("n", "<leader>tl", ":set relativenumber!<CR>", { silent = true, desc = "[T]oggle [L]ine numbers" })

        -- a handy wrap remapping
        map("n", "k", "v:count == 0 ? 'gk' : 'k'", { expr = true, silent = true })
        map("n", "j", "v:count == 0 ? 'gj' : 'j'", { expr = true, silent = true })

        -- Diagnostic keymaps
        map("n", "<leader>[", vim.diagnostic.goto_prev, { desc = "Go to previous diagnostic message" })
        map("n", "<leader>]", vim.diagnostic.goto_next, { desc = "Go to next diagnostic message" })
        map("n", "<leader>e", vim.diagnostic.open_float, { desc = "Open floating diagnostic message" })
        map("n", "<leader>q", vim.diagnostic.setloclist, { desc = "Open diagnostics list" })

        -- Terminal keymaps
        map("t", "<Esc>", "<C-\\><C-n>", { silent = true, desc = "Exit terminal mode" })

        -- Since I will mostly use this in my obsidian vault, I'm going to continue to use the <leader>o convention.
        -- these functions are included in functions.lua
        map("n", "<leader>oc", ":lua toggle_checkbox()<CR>", { noremap = true, desc = "[O]bsidian [C]heckbox" })
        map("n", "<leader>ol", ":lua wrap_wiki_link_normal()<CR>", { noremap = true, desc = "[O]bsidian [L]ink" })
        map("x", "<leader>ol", ":lua wrap_wiki_link_visual()<CR>", { noremap = true, desc = "[O]bsidian [L]ink" })
      '';
  };
}
