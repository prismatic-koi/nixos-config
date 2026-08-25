{
  config,
  lib,
  pkgs,
  ...
}:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.zen-mode-nvim;
      type = "lua";
      config =
        # lua
        ''
          -- The built-in kitty integration (zen-mode.nvim 1.4.1,
          -- lua/zen-mode/plugins.lua lines 36-48) resolves the control socket
          -- with vim.fn.expand("$KITTY_LISTEN_ON"), which is hard-coded and
          -- cannot see a socket exported only through tmux's session
          -- environment. Disable it and drive the font size ourselves from
          -- on_open/on_close below, resolving the socket at call time so a
          -- re-attach to a different kitty window still works. See issue #2860.
          --
          -- Resolves the kitty remote-control socket, trying in order:
          --   1. $KITTY_LISTEN_ON, when kitty set it directly (bare kitty window).
          --   2. `tmux show-environment KITTY_LISTEN_ON`, for a pane inside tmux,
          --      where the tmux server's fixed startup environment is refreshed
          --      on attach via `update-environment` (see tmux.nix). The
          --      `-KITTY_LISTEN_ON` removal form (variable unset) is treated as
          --      absent, not as a value.
          -- Returns nil quietly when no socket is available (e.g. neovim
          -- outside kitty entirely) - callers must not error or print in that
          -- case, only report a genuine failed `kitty @` call.
          local function resolve_kitty_socket()
            local direct = vim.fn.expand("$KITTY_LISTEN_ON")
            if direct ~= "" and direct ~= "$KITTY_LISTEN_ON" then
              return direct
            end
            if vim.fn.exists("$TMUX") == 0 then
              return nil
            end
            local ok, output = pcall(vim.fn.system, { "tmux", "show-environment", "KITTY_LISTEN_ON" })
            if not ok or vim.v.shell_error ~= 0 then
              return nil
            end
            output = vim.trim(output)
            if output == "" or vim.startswith(output, "-KITTY_LISTEN_ON") then
              return nil
            end
            local socket = output:match("^KITTY_LISTEN_ON=(.*)$")
            if socket == nil or socket == "" then
              return nil
            end
            return socket
          end

          -- Runs `kitty @ --to <socket> set-font-size <size>`, resolving the
          -- socket fresh on every call. Silent when no socket is available.
          -- A genuine `kitty @` failure (socket resolved but the call itself
          -- failed) is reported once, at debug level, instead of on every
          -- toggle.
          local function set_kitty_font_size(size)
            local socket = resolve_kitty_socket()
            if socket == nil then
              return
            end
            local ok, output =
              pcall(vim.fn.system, { "kitty", "@", "--to", socket, "set-font-size", tostring(size) })
            if not ok or vim.v.shell_error ~= 0 then
              vim.notify("zen-mode: kitty @ set-font-size failed: " .. tostring(output), vim.log.levels.DEBUG)
            end
          end

          require("zen-mode").setup({
            window = {
              backdrop = 1,
              width = 1,
              options = {
                number = false,
              },
            },
          	plugins = {
              gitsigns = { enabled = false },
          		tmux = { enabled = true },
          		kitty = { enabled = false },
          	},
            on_open = function(win)
              local gs = require("gitsigns")
              local config = require("gitsigns.config").config
              config.current_line_blame = false
              config.current_line_blame_opts.virt_text = false
              gs.refresh()
              set_kitty_font_size("+2")
            end,
            on_close = function(win)
              local gs = require("gitsigns")
              local config = require("gitsigns.config").config
              config.current_line_blame = true
              config.current_line_blame_opts.virt_text = true
              gs.refresh()
              set_kitty_font_size(0)
            end,
          })
          vim.keymap.set("n", "<leader>z", vim.cmd.ZenMode, { desc = "[Z]enMode" })
        '';
    }
  ];
}
