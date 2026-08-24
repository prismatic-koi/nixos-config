{
  pkgs,
  lib,
  config,
  ...
}:
let
  theme = config.theme;
in
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins =
    lib.mkIf (config.theme.name == "gruvbox-light" || config.theme.name == "gruvbox-dark")
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
