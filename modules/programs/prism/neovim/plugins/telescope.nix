{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username} = {
    home.packages = [
      pkgs.fzf
    ];
    programs.neovim.plugins = [
      {
        plugin = pkgs.vimPlugins.telescope-nvim;
        type = "lua";
        config =
          # lua
          ''
            local square_borders = { "─", "│", "─", "│", "┌", "┐", "┘", "└" }

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
