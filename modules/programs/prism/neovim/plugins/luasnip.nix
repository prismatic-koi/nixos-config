{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.luasnip;
      type = "lua";
      config =
        # lua
        ''
          require("luasnip").setup({})
          local ls = require("luasnip")
          local s = ls.snippet
          local sn = ls.snippet_node
          local t = ls.text_node
          local d = ls.dynamic_node

          -- Note: I used to load in a plugin called friendly-snippets for more snippets
          -- but now I prefer to just define a subset of those myself
          -- you can find ideas here: https://github.com/rafamadriz/friendly-snippets/tree/main

          -- my snippets
          ls.add_snippets("markdown", {
          	-- wikidate snippet
          	s(
          		"wikidate",
          		d(1, function(args, parent)
          			local env = parent.snippet.env
          			return sn(
          				nil,
          				t({ "[[" .. env.CURRENT_YEAR .. "-" .. env.CURRENT_MONTH .. "-" .. env.CURRENT_DATE .. "]]" })
          			)
          		end, {})
          	),
          })
        '';
    }
  ];
}
