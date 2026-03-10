{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.leap-nvim;
      type = "lua";
      config =
        # lua
        ''
          vim.keymap.set({'n', 'x', 'o'}, 's', '<Plug>(leap)')
          vim.keymap.set('n',             'S', '<Plug>(leap-from-window)')
        '';
    }
    {
      plugin = pkgs.vimPlugins.flit-nvim;
      type = "lua";
      config =
        # lua
        ''
          require("flit").setup()
        '';
    }
    pkgs.vimPlugins.vim-repeat
  ];
}
