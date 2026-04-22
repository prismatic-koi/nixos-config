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

          -- f/F/t/T via leap (replaces flit.nvim)
          do
            local function ft(key_specific_args)
              require('leap').leap(
                vim.tbl_deep_extend('keep', key_specific_args, {
                  inputlen = 1,
                  inclusive = true,
                  opts = {
                    labels = "",
                    safe_labels = vim.fn.mode(1):match('o') and "" or nil,
                  },
                })
              )
            end

            local clever_f = require('leap.user').with_traversal_keys('f', 'F')
            local clever_t = require('leap.user').with_traversal_keys('t', 'T')

            vim.keymap.set({'n', 'x', 'o'}, 'f', function() ft { opts = clever_f } end)
            vim.keymap.set({'n', 'x', 'o'}, 'F', function() ft { backward = true,  opts = clever_f } end)
            vim.keymap.set({'n', 'x', 'o'}, 't', function() ft { offset = -1,      opts = clever_t } end)
            vim.keymap.set({'n', 'x', 'o'}, 'T', function() ft { backward = true, offset = 1, opts = clever_t } end)
          end
        '';
    }
    pkgs.vimPlugins.vim-repeat
  ];
}
