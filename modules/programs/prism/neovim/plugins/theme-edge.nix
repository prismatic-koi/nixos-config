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
    lib.mkIf (config.theme.name == "edge")
      [
        {
          plugin = pkgs.vimPlugins.edge;
          type = "lua";
          config =
            # lua
            ''
              vim.g.edge_style = "default"
              vim.o.background = "${theme.type}"

              -- edge is a vimscript colour scheme without a lua setup() API,
              -- so highlight overrides are applied via vim.api.nvim_set_hl after
              -- the scheme loads. Wrap them in a ColorScheme autocmd so they
              -- re-apply on reload (upstream-recommended pattern).
              local edge_obsidian_highlights = vim.api.nvim_create_augroup(
                "EdgeObsidianHighlights",
                { clear = true }
              )
              vim.api.nvim_create_autocmd("ColorScheme", {
                group = edge_obsidian_highlights,
                pattern = "edge",
                callback = function()
                  vim.api.nvim_set_hl(0, "ObsidianTodo", {
                    fg = "${theme.hues.purple}",
                    bold = true,
                  })
                  vim.api.nvim_set_hl(0, "ObsidianDone", {
                    fg = "${theme.hues.green}",
                    bold = true,
                  })
                  vim.api.nvim_set_hl(0, "ObsidianRightArrow", {
                    fg = "${theme.hues.purple}",
                    bold = true,
                  })
                  vim.api.nvim_set_hl(0, "ObsidianTilde", {
                    fg = "${theme.hues.orange}",
                    bold = true,
                  })
                  vim.api.nvim_set_hl(0, "ObsidianRefText", {
                    fg = "${theme.hues.blue}",
                  })
                  vim.api.nvim_set_hl(0, "ObsidianExtLinkIcon", {
                    fg = "${theme.hues.blue}",
                  })
                  vim.api.nvim_set_hl(0, "ObsidianTag", {
                    fg = "${theme.hues.blue}",
                    italic = true,
                  })
                  vim.api.nvim_set_hl(0, "ObsidianBullet", {
                    fg = "${theme.neutrals.background_5}",
                    bold = true,
                  })
                  vim.api.nvim_set_hl(0, "ObsidianHighlightText", {
                    fg = "${theme.neutrals.background_0}",
                    bg = "${theme.roles.primary}",
                  })
                end,
              })

              vim.cmd("colorscheme edge")
            '';
        }
      ];
}
