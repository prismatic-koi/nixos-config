{
  config,
  pkgs,
  lib,
  ...
}:
let
  theme = config.theme;
  isEverforest = theme.name == "everforest-dark" || theme.name == "everforest-light";
  # Explicit theme: "auto" resolves before the colour scheme loads and
  # falls back to the dark default highlights.
  everforestTheme =
    # lua
    ''
      (function()
      	local fg, bg = "${theme.neutrals.foreground}", "${theme.neutrals.background_1}"
      	local function mode(accent)
      		return {
      			a = { fg = bg, bg = accent, gui = "bold" },
      			b = { fg = fg, bg = bg },
      			c = { fg = fg, bg = bg },
      		}
      	end
      	return {
      		normal = mode("${theme.roles.primary}"),
      		insert = mode("${theme.hues.blue}"),
      		visual = mode("${theme.hues.red}"),
      		replace = mode("${theme.hues.yellow}"),
      		command = mode("${theme.hues.green}"),
      		inactive = mode("${theme.neutrals.foreground}"),
      	}
      end)()
    '';
in
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    # dependencies
    pkgs.vimPlugins.nvim-web-devicons
    {
      plugin = pkgs.vimPlugins.lualine-nvim;
      type = "lua";
      config =
        # lua
        ''
          require("lualine").setup({
          	options = {
          		icons_enabled = true,
          		theme = ${if isEverforest then everforestTheme else ''"auto"''},
          		component_separators = "|",
          		section_separators = "",
          		refresh = {
          			statusline = 100,
          			tabline = 100,
          			winbar = 100,
          		},
          		sections = {
          			lualine_a = { "mode" },
          			lualine_b = { "branch", "diff", "diagnostics" },
          			lualine_c = { "filename" },
          			lualine_x = { "filetype" },
          			lualine_y = { "progress" },
          			lualine_z = { "location" },
          		},
          		inactive_sections = {
          			lualine_a = {},
          			lualine_b = {},
          			lualine_c = { "filename" },
          			lualine_x = { "location" },
          			lualine_y = {},
          			lualine_z = {},
          		},
          	},
          })
        '';
    }
  ];
}
