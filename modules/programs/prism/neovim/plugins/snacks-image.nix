{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username} = {
    programs.neovim.plugins = [
      {
        plugin = pkgs.vimPlugins.snacks-nvim;
        type = "lua";
        config =
          # lua
          ''
            -- Each top-level key in this table enables its snacks module, also without `enabled = true`.
            require("snacks").setup({
            	image = {
            		enabled = true,
            		doc = {
            			enabled = true,
            			inline = true,
            		},
            	},
            })
          '';
      }
    ];
  };
}
