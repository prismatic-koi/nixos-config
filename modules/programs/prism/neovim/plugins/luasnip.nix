{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.cmp_luasnip;
      type = "lua";
      config =
        # lua
        ''
          require("cmp").setup()
        '';
    }
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
          local i = ls.insert_node
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
          	-- header snippets
          	s("h1", { t("# "), i(0) }),
          	s("h2", { t("## "), i(0) }),
          	s("h3", { t("### "), i(0) }),
          	s("h4", { t("#### "), i(0) }),
          	s("h5", { t("##### "), i(0) }),
          	s("h6", { t("###### "), i(0) }),
          	-- image snippet
          	s("img", { t("!["), i(1, "alt text"), t("]("), i(2, "path"), t(") "), i(0) }),
          	-- formatting snippets
          	s("strikethrough", { t("~~"), i(1), t("~~ "), i(0) }),
          	s({ trig = "bold", name = "bold" }, { t("**"), i(1), t("** "), i(0) }),
          	s({ trig = "b", name = "bold" }, { t("**"), i(1), t("** "), i(0) }),
          	s({ trig = "i", name = "italic" }, { t("*"), i(1), t("* "), i(0) }),
          	s({ trig = "italic", name = "italic" }, { t("*"), i(1), t("* "), i(0) }),
          	s({ trig = "bi", name = "bold and italic" }, { t("***"), i(1), t("*** "), i(0) }),
          	s({ trig = "bold and italic", name = "bold and italic" }, { t("***"), i(1), t("*** "), i(0) }),
          	-- code snippets
          	s("code", { t("`"), i(1), t("` "), i(0) }),
          	s("codeblock", { t("```"), i(1, "language"), t({ "", "" }), i(0), t({ "", "```" }) }),
          })
        '';
    }
  ];
}
