{ config, pkgs, ... }:
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
          		theme = "auto",
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
