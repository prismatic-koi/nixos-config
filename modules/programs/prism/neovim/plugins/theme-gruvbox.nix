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
              	overrides = {
              		ObsidianTodo = { fg = "${theme.hues.orange}", bold = true },
              		ObsidianDone = { fg = "${theme.hues.green}", bold = true },
              		ObsidianRightArrow = { fg = "${theme.hues.orange}", bold = true },
              		ObsidianTilde = { fg = "${theme.hues.red}", bold = true },
              		ObsidianImportant = { fg = "${theme.hues.red}", bold = true },
              		ObsidianBullet = { fg = "${theme.neutrals.background_5}" },
              		ObsidianRefText = { fg = "${theme.hues.blue}", underline = true },
              		ObsidianExtLinkIcon = { fg = "${theme.hues.blue}" },
              		ObsidianTag = { fg = "${theme.hues.blue}", italic = true },
              		ObsidianBlockID = { fg = "${theme.hues.blue}", italic = true },
              		ObsidianHighlightText = { bg = "${theme.backgrounds.bg_visual}" },
              	},
              })
              vim.o.background = "${theme.type}"
              vim.cmd([[colorscheme gruvbox]])
            '';
        }
      ];
}
