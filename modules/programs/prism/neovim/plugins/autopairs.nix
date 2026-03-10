{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.nvim-autopairs;
      type = "lua";
      config =
        # lua
        ''
          require("nvim-autopairs").setup()
          -- don't close quotes
          require("nvim-autopairs").remove_rule('"')
        '';
    }
  ];
}
