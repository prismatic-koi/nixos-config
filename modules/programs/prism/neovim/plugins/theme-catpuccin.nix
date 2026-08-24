{
  pkgs,
  lib,
  config,
  ...
}:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins =
    let
      isCatppuccin = lib.hasPrefix "catppuccin" config.theme.name;
      catppuccinFlavour = lib.removePrefix "catppuccin-" config.theme.name;
    in
    lib.mkIf isCatppuccin [
      {
        plugin = pkgs.vimPlugins.catppuccin-nvim;
        type = "lua";
        config =
          # lua
          ''
            require("catppuccin").setup({
            	flavour = "${catppuccinFlavour}",
            	transparent_background = true,
            })
            vim.cmd("colorscheme catppuccin")
          '';
      }
    ];
}
