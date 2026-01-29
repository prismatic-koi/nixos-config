{ ... }:
{
  # Setting some helpful autocommands
  home-manager.users.ben.programs.neovim = {
    extraLuaConfig =
      # lua
      ''
        local function augroup(name)
        	return vim.api.nvim_create_augroup("prismatic-koi_" .. name, { clear = true })
        end

        -- highlight on yank
        local highlight_group = vim.api.nvim_create_augroup("YankHighlight", { clear = true })
        vim.api.nvim_create_autocmd("TextYankPost", {
        	callback = function()
        		vim.highlight.on_yank()
        	end,
        	group = highlight_group,
        	pattern = "*",
        })

        -- resize splits if window got resized
        vim.api.nvim_create_autocmd({ "VimResized" }, {
        	group = augroup("resize_splits"),
        	callback = function()
        		vim.cmd("tabdo wincmd =")
        	end,
        })

        -- check for spell in text filetypes
        vim.api.nvim_create_autocmd("FileType", {
        	group = augroup("wrap_spell"),
        	pattern = { "gitcommit", "markdown" },
        	callback = function()
        		vim.opt_local.spell = true
        	end,
        })

        -- format on save
        vim.api.nvim_create_autocmd("BufWritePre", {
        	group = augroup("format_on_save"),
        	callback = function()
        		vim.lsp.buf.format()
        	end,
        })

        -- auto reload buffer when file changes externally
        vim.api.nvim_create_autocmd({ "FocusGained", "BufEnter", "CursorHold", "CursorHoldI" }, {
        	group = augroup("auto_reload"),
        	callback = function()
        		if vim.fn.mode() ~= "c" and vim.fn.getcmdwintype() == "" then
        			vim.cmd("checktime")
        		end
        	end,
        })

        -- notify when file changes externally
        vim.api.nvim_create_autocmd("FileChangedShellPost", {
        	group = augroup("file_changed_notify"),
        	callback = function()
        		vim.notify("File changed on disk. Buffer reloaded.", vim.log.levels.WARN)
        	end,
        })
      '';
  };
}
