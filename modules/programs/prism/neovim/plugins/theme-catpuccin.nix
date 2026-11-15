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
            	custom_highlights = function(colors)
            		return {
            			ObsidianTodo = { fg = colors.peach, bold = true },
            			ObsidianDone = { fg = colors.green, bold = true },
            			ObsidianRightArrow = { fg = colors.peach, bold = true },
            			ObsidianTilde = { fg = colors.red, bold = true },
            			ObsidianImportant = { fg = colors.red, bold = true },
            			ObsidianBullet = { fg = colors.overlay1 },
            			ObsidianRefText = { fg = colors.blue, underline = true },
            			ObsidianExtLinkIcon = { fg = colors.blue },
            			ObsidianTag = { fg = colors.blue, italic = true },
            			ObsidianBlockID = { fg = colors.blue, italic = true },
            			ObsidianHighlightText = { bg = colors.surface1 },
            		}
            	end,
            })
            vim.cmd("colorscheme catppuccin")
          '';
      }
    ];
}
