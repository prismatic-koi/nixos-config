{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      # plugin = pkgs.vimPlugins.nvim-sops;
      plugin = pkgs.vimPlugins.nvim-sops;
      type = "lua";
      config =
        # lua
        ''
          require("nvim_sops").setup({
          	enabled = true,
          	debug = false,
          })
          vim.keymap.set("n", "<leader>ef", vim.cmd.SopsEncrypt, { desc = "[E]ncrypt [F]ile" })
          vim.keymap.set("n", "<leader>df", vim.cmd.SopsDecrypt, { desc = "[D]ecrypt [F]ile" })
        '';
    }
  ];
}
