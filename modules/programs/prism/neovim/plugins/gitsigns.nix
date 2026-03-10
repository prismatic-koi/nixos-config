{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.gitsigns-nvim;
      type = "lua";
      config =
        # lua
        ''
          require("gitsigns").setup({
          	signcolumn = true,
          	current_line_blame = true,
          	current_line_blame_opts = {
          		virt_text = true,
          		virt_text_pos = "eol",
          		delay = 200,
          		ignore_whitespace = false,
          	},
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
