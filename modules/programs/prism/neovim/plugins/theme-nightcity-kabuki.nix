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
  theme = config.themev2;
in
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins =
    lib.mkIf (config.themev2.name == "nightcity-kabuki")
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
              		groups.StatusLine = { fg = "${theme.neutrals.foreground}", bg = "${theme.neutrals.background_1}" }
              		-- obsidian
              		groups.ObsidianTodo = { fg = "${theme.roles.primary}", style = "bold" }
              		groups.ObsidianDone = { fg = "${theme.roles.primary}", style = "bold" }
              		groups.ObsidianTilde = { fg = "${theme.hues.red}", style = "bold" }
              		groups.ObsidianRefText = { fg = "${theme.roles.primary}" }
              		groups.ObsidianExtLinkIcon = { fg = "${theme.roles.primary}" }
              		groups.ObsidianTag = { fg = "${theme.roles.secondary}", style = "italic" }
              		groups["@markup.heading.1.markdown"] = { fg = "${theme.hues.orange}", style = "bold" }
              		groups["@markup.heading.2.markdown"] = { fg = "${theme.hues.red}", style = "bold" }
              		groups["@markup.heading.3.markdown"] = { fg = "${theme.hues.purple}", style = "bold" }
              	end,
              })
              vim.cmd("colorscheme nightcity")
            '';
        }
      ];
}
