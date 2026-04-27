{
  pkgs,
  lib,
  config,
  ...
}:
let
  nightcity-theme = pkgs.vimUtils.buildVimPlugin {
    pname = "nightcity-theme";
    version = "unstable";
    src = pkgs.fetchFromGitHub {
      owner = "cryptomilk";
      repo = "nightcity.nvim";
      rev = "c38ec1f32f6224da7b9eaf7bb27a8133bcc4c016";
      hash = "sha256-/ATSVsUaiy6yMREVyxFRJZxuWFbcCKxwZiy3EXsssoI=";
    };
  };
  theme = config.theme;
in
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins =
    lib.mkIf (config.theme.name == "nightcity-kabuki")
      [
        {
          plugin = nightcity-theme;
          type = "lua";
          config =
            # lua
            ''
              require("nightcity").setup({
              	style = "kabuki",
              	on_highlights = function(groups, c)
              		-- no bg for String
              		groups.String = { fg = c.text }
              		-- statusline consistent with other themes + tmux
              		groups.StatusLine = { fg = "${theme.foreground}", bg = "${theme.bg1}" }
              		-- obsidian
              		groups.ObsidianTodo = { fg = "${theme.primary}", style = "bold" }
              		groups.ObsidianDone = { fg = "${theme.primary}", style = "bold" }
              		groups.ObsidianTilde = { fg = "${theme.red}", style = "bold" }
              		groups.ObsidianRefText = { fg = "${theme.primary}" }
              		groups.ObsidianExtLinkIcon = { fg = "${theme.primary}" }
              		groups.ObsidianTag = { fg = "${theme.secondary}", style = "italic" }
              		groups["@markup.heading.1.markdown"] = { fg = "${theme.orange}", style = "bold" }
              		groups["@markup.heading.2.markdown"] = { fg = "${theme.red}", style = "bold" }
              		groups["@markup.heading.3.markdown"] = { fg = "${theme.purple}", style = "bold" }
              	end,
              })
              vim.cmd("colorscheme nightcity")
            '';
        }
      ];
}
