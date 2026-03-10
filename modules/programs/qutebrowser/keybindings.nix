{
  config,
  lib,
  ...
}:
{
  config = lib.mkIf config.nx.programs.qutebrowser.enable {
    home-manager.users.${config.nx.username} = {
      # keybindings
      # I did used to use a combination of config.bind and c.bindings
      # but my 'ch' with no leader did not work in config.bind
      # and having a combination of binding strategies caused them to interfere
      programs.qutebrowser.keyBindings = {
        normal = {
          # unbind
          "<Ctrl-h>" = "nop";
          "M" = "nop";
          "m" = "nop";
          # close tabs left and right
          "ch" = "tab-only --next";
          "cl" = "tab-only --prev";
          "tg" = "tab-give";
          # open clipboard item shortcuts
          "p" = "open -- {clipboard}";
          "P" = "open -t -- {clipboard}";
          # edit current url
          "e" = "cmd-set-text :open {url:pretty}";
          "E" = "cmd-set-text :open -t {url:pretty}";
          # edit text
          "<Ctrl-e>" = "edit-text";
          # bitwarden bindings
          "<Space>ll" = "spawn --userscript bitwarden --totp";
          "<Space>lu" = "spawn --userscript bitwarden --username-only";
          "<Space>lp" = "spawn --userscript bitwarden --password-only";
          "<Space>lt" = "spawn --userscript bitwarden --totp-only";
          "<Space>lb" = "spawn --userscript open-bitwarden";
          # youtube music download
          "<Space>md" = "spawn --userscript ytm-download";
          "<Space>y" = "yank selection";
          "<Space>ff" = "spawn --userscript open-firefox";
        };
      };
    };
  };
}
