{
  config,
  lib,
  pkgs,
  ...
}:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.undotree;
      type = "lua";
      config =
        # lua
        ''
          vim.keymap.set("n", "<leader>u", vim.cmd.UndotreeToggle, { desc = "[U]ndo Tree" })
        '';
    }
  ];
}
