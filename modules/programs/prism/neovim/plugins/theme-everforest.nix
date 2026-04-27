{
  config,
  pkgs,
  lib,
  ...
}:
let
  everforest-nvim = pkgs.vimUtils.buildVimPlugin {
    pname = "everforest-nvim";
    version = "unstable";
    src = pkgs.fetchFromGitHub {
      owner = "neanias";
      repo = "everforest-nvim";
      rev = "ed4ba26c911696d69cfda26014ec740861d324e1";
      hash = "sha256-kVn6rUc26PtoqzKW/MNuks85sTLYx1lEE/l+7W0bDfg=";
    };
  };
in
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins =
    lib.mkIf (config.theme.name == "everforest")
      [
        {
          plugin = everforest-nvim;
          type = "lua";
          config =
            # lua
            ''
              local everforest = require("everforest")
              everforest.setup({
              	on_highlights = function(hl, palette)
              		-- obsidian.nvim colours
              		hl["ObsidianTodo"] = {
              			fg = palette.orange,
              			bg = palette.none,
              			bold = true,
              		}
              		hl["ObsidianDone"] = {
              			fg = palette.green,
              			bg = palette.none,
              			bold = true,
              		}
              		hl["ObsidianRightArrow"] = {
              			fg = palette.orange,
              			bg = palette.none,
              			bold = true,
              		}
              		hl["ObsidianTilde"] = {
              			fg = palette.red,
              			bg = palette.none,
              			bold = true,
              		}
              		hl["ObsidianRefText"] = {
              			fg = palette.blue,
              			bg = palette.none,
              			-- underline = true,
              		}
              		hl["ObsidianExtLinkIcon"] = {
              			fg = palette.blue,
              			bg = palette.none,
              		}
              		hl["ObsidianTag"] = {
              			fg = palette.blue,
              			bg = palette.none,
              			italic = true,
              		}
              		hl["ObsidianBullet"] = {
              			fg = palette.grey0,
              			bg = palette.none,
              		}
              	end,
              })
              everforest.load()
            '';
        }
      ];
}
