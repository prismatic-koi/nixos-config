{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.nvim-treesitter.withPlugins (
        treesitter-plugins: with treesitter-plugins; [
          bash
          cmake
          css
          dockerfile
          gitcommit
          gitignore
          graphql
          hcl
          helm
          html
          javascript
          jq
          json
          json5
          latex
          lua
          markdown
          markdown_inline
          nix
          python
          sql
          tmux
          toml
          typescript
          vim
          xml
          yaml
        ]
      );
      type = "lua";
      config =
        # lua
        ''
          -- Enable treesitter highlighting for all supported filetypes
          vim.api.nvim_create_autocmd('FileType', {
            callback = function()
              local buf = vim.api.nvim_get_current_buf()
              -- Check if treesitter parser is available for this buffer
              pcall(vim.treesitter.start, buf)
            end,
          })
        '';
    }
  ];
}
