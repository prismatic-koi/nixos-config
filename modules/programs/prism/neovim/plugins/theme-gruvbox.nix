{
  pkgs,
  lib,
  config,
  ...
}:
let
  theme = config.themev2;
in
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins =
    lib.mkIf (config.themev2.name == "gruvbox-light" || config.themev2.name == "gruvbox-dark")
      [
        {
          plugin = pkgs.vimPlugins.gruvbox-nvim;
          type = "lua";
          config =
            # lua
            ''
              require("gruvbox").setup({
              	overrides = {},
              })
              vim.o.background = "${theme.type}"
              vim.cmd([[colorscheme gruvbox]])
            '';
        }
      ];
}
