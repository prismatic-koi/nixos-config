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
    lib.mkIf (config.theme.name == "onedark")
      [
        {
          plugin = pkgs.vimPlugins.onedark-nvim;
          type = "lua";
          config =
            # lua
            ''
              require("onedark").setup({
              	highlights = {
              		ObsidianTodo = {
              			fg = "${theme.hues.purple}",
              			bg = "none",
              			fmt = "bold",
              		},
              		ObsidianDone = {
              			fg = "${theme.hues.green}",
              			bg = "none",
              			fmt = "bold",
              		},
              		ObsidianRightArrow = {
              			fg = "${theme.hues.purple}",
              			bg = "none",
              			fmt = "bold",
              		},
              		ObsidianTilde = {
              			fg = "${theme.hues.orange}",
              			bg = "none",
              			fmt = "bold",
              		},
              		ObsidianRefText = {
              			fg = "${theme.hues.blue}",
              			bg = "none",
              		},
              		ObsidianExtLinkIcon = {
              			fg = "${theme.hues.blue}",
              			bg = "none",
              		},
              		ObsidianTag = {
              			fg = "${theme.hues.blue}",
              			bg = "none",
              			fmt = "italic",
              		},
                  ObsidianBullet = {
                    fg = "${theme.neutrals.background_5}",
                    bg = "none",
                    fmt = "bold",
                  },
                  ObsidianHighlightText = {
                    fg = "${theme.neutrals.background_0}",
                    bg = "${theme.roles.primary}",
                  }
              	},
              })
              vim.cmd("colorscheme onedark")
            '';
        }
      ];
}
