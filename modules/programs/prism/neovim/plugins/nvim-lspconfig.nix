{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username} = {
    home.packages = with pkgs; [
      helm-ls
      lua-language-server
      nil
      rust-analyzer
      typescript-language-server
      yaml-language-server
    ];
    programs.neovim.plugins = [
      # LSP and completions for injected langs
      pkgs.vimPlugins.otter-nvim
      pkgs.vimPlugins.cmp-nvim-lsp
      pkgs.vimPlugins.vim-helm
      # LSP
      {
        plugin = pkgs.vimPlugins.nvim-lspconfig;
        type = "lua";
        config =
          # lua
          ''
            local default_capabilities = require("cmp_nvim_lsp").default_capabilities()

            function add_lsp(name, options)
            	options = options or {}
            	if not options.capabilities then
            		options.capabilities = default_capabilities
            	end

            	local cmd = options.cmd or vim.lsp.config[name].cmd
            	if cmd and vim.fn.executable(cmd[1]) == 1 then
            		vim.lsp.config[name] = vim.tbl_extend('force', vim.lsp.config[name] or {}, options)
            		vim.lsp.enable(name)
            	end
            end

            add_lsp('cssls')
            add_lsp('dockerls')
            add_lsp('eslint')
            add_lsp('helm_ls')
            add_lsp('html')
            add_lsp('jsonls')
            add_lsp('lua_ls')
            add_lsp('nil_ls', {
            	settings = { ["nil"] = {
            		formatting = { command = { "nixfmt" } },
            	} },
            })
            add_lsp('pylsp')
            add_lsp('ts_ls')
            add_lsp('yamlls', {
            	settings = { ["yamlls"] = {
            		keyOrdering = false,
            	} },
            })
            -- keymaps
            vim.keymap.set("n", "<leader>rn", vim.lsp.buf.rename, { buffer = bufnr, desc = "[R]e[n]ame" })
            vim.keymap.set("n", "<leader>ca", vim.lsp.buf.code_action, { buffer = bufnr, desc = "[C]ode [A]ction" })

            vim.keymap.set("n", "gd", vim.lsp.buf.definition, { buffer = bufnr, desc = "[G]oto [D]efinition" })
            vim.keymap.set("n", "gD", vim.lsp.buf.declaration, { buffer = bufnr, desc = "[G]oto [D]eclaration" })
            vim.keymap.set("n", "gi", vim.lsp.buf.implementation, { buffer = bufnr, desc = "[G]oto [I]mplementation" })
            vim.keymap.set("n", "gr", vim.lsp.buf.references, { buffer = bufnr, desc = "[G]oto [R]eferences" })
            vim.keymap.set("n", "K", vim.lsp.buf.hover, { buffer = bufnr, desc = "[K]ind (Hover Documentation)" })

            vim.keymap.set("n", "<leader>D", vim.lsp.buf.type_definition, { buffer = bufnr, desc = "Type [D]efinition" })
            vim.keymap.set(
            	"n",
            	"<leader>ds",
            	require("telescope.builtin").lsp_document_symbols,
            	{ buffer = bufnr, desc = "[D]ocument [S]ymbols" }
            )
            vim.keymap.set(
            	"n",
            	"<leader>ws",
            	require("telescope.builtin").lsp_workspace_symbols,
            	{ buffer = bufnr, desc = "[W]orkspace [S]ymbols" }
            )
            -- format
            -- vim.keymap.set('n', '<leader>f',
            --   vim.lsp.buf.formatting, { buffer = bufnr, desc = '[F]ormat'})
          '';
      }
    ];
  };
}
