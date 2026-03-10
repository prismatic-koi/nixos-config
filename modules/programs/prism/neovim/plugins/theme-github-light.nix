{
  pkgs,
  lib,
  config,
  ...
}:
let
  github-nvim-theme = pkgs.vimUtils.buildVimPlugin {
    name = "github-nvim-theme";
    src = pkgs.fetchFromGitHub {
      owner = "projekt0n";
      repo = "github-nvim-theme";
      rev = "0e4636f556880d13c00d8a8f686fae8df7c9845f";
      hash = "sha256-EreIuni6/XR0428rO4Lbi2usIreOyPWKm7kJJA2Nwqo=";
    };
  };
  theme = config.theme;
in
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins =
    lib.mkIf (config.theme.name == "github-light")
      [
        {
          plugin = github-nvim-theme;
          type = "lua";
          config =
            # lua
            ''
              require("github-theme").setup({
              	groups = {
              		all = {
              			ObsidianTodo = {
              				fg = "${theme.blue}",
              				bg = "none",
              				style = "bold",
              			},
              			ObsidianDone = {
              				fg = "${theme.green}",
              				bg = "none",
              				style = "bold",
              			},
              			ObsidianRightArrow = {
              				fg = "${theme.blue}",
              				bg = "none",
              				style = "bold",
              			},
              			ObsidianTilde = {
              				fg = "${theme.orange}",
              				bg = "none",
              				style = "bold",
              			},
              			ObsidianRefText = {
              				fg = "${theme.blue}",
              				bg = "none",
              				style = "NONE", -- no underline for obsidian links
              			},
              			ObsidianExtLinkIcon = {
              				fg = "${theme.blue}",
              				bg = "none",
              			},
              			ObsidianTag = {
              				fg = "${theme.blue}",
              				bg = "none",
              				style = "italic",
              			},
              			["@markup.heading.1.markdown"] = {
              				fg = "${theme.blue}",
              				bg = "none",
              				style = "bold",
              			},
              			["@markup.heading.2.markdown"] = {
              				fg = "${theme.green}",
              				bg = "none",
              				style = "bold",
              			},
              			["@markup.heading.3.markdown"] = {
              				fg = "${theme.purple}",
              				bg = "none",
              				style = "bold",
              			},
              		},
              	},
              })
              vim.cmd("colorscheme github_light")
            '';
        }
      ];
}
