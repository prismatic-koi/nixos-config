{
  config,
  pkgs,
  ...
}:
let
  live-server-nvim = pkgs.vimUtils.buildVimPlugin {
    pname = "live-server-nvim";
    version = "unstable";
    src = pkgs.fetchFromGitHub {
      owner = "selimacerbas";
      repo = "live-server.nvim";
      rev = "446b2211de3819f67e807b768ebd200054e1cf06";
      hash = "sha256-AYGkm9mndYLcr1cRvNCemzvZbjby5z+wWMEaSvGVbt8=";
    };
  };
  markdown-preview-nvim = pkgs.vimUtils.buildVimPlugin {
    pname = "markdown-preview-nvim";
    version = "unstable";
    nvimSkipModules = [ "markdown_preview" ];
    src = pkgs.fetchFromGitHub {
      owner = "selimacerbas";
      repo = "markdown-preview.nvim";
      rev = "d211d554e1e7f57088419b2d9349cf02eb311271";
      hash = "sha256-tuLgb2P85Kv9QrN0hXvWd+EyMycLIhY0goNx7bkq86s=";
    };
  };
in
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    { plugin = live-server-nvim; }
    {
      plugin = markdown-preview-nvim;
      type = "lua";
      config =
        # lua
        ''
          require("markdown_preview").setup({
            instance_mode = "takeover",
            port = 0,
            open_browser = true,
            debounce_ms = 300,
            scroll_sync = true,
          })

          vim.keymap.set("n", "<leader>mps", "<cmd>MarkdownPreview<cr>", { desc = "[M]arkdown [P]review [S]tart" })
          vim.keymap.set("n", "<leader>mpS", "<cmd>MarkdownPreviewStop<cr>", { desc = "[M]arkdown [P]review [S]top" })
          vim.keymap.set("n", "<leader>mpr", "<cmd>MarkdownPreviewRefresh<cr>", { desc = "[M]arkdown [P]review [R]efresh" })
        '';
    }
  ];
}
