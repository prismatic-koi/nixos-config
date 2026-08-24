{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.otter-nvim;
      type = "lua";
      config =
        # lua
        ''
          require("otter").setup({
            buffers = { set_filetype = true },
          })

          vim.api.nvim_create_autocmd("FileType", {
            pattern = { "nix", "markdown" },
            callback = function()
              require("otter").activate(nil, true, false)
            end,
          })
        '';
    }
  ];
}
