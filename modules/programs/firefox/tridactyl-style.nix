{
  config,
  lib,
  ...
}:
with config.themev2;
# v1 `grey0` (scrollbar track, no themev2 slot) maps to neutrals.background_5
# (issue #2814), matching the qutebrowser/greasemonkey precedent.
{
  config = lib.mkIf config.nx.programs.firefox.enable {
    home-manager.users.${config.nx.username}.home.file.".config/tridactyl/themes/customtheme.css".text =
      # css
      ''
        :root {
            --tridactyl-fg: ${neutrals.foreground};
            --tridactyl-bg: ${neutrals.background_0};

            --tridactyl-scrollbar-color: ${neutrals.background_5} ${neutrals.foreground};

            --tridactyl-cmdl-fg: ${neutrals.foreground};
            --tridactyl-cmdl-bg: ${neutrals.background_0};

            --tridactyl-header-first-bg: ${neutrals.background_1};
            --tridactyl-header-second-bg: ${neutrals.background_1};
            --tridactyl-header-third-bg: ${neutrals.background_1};

            --tridactyl-cmplt-border-top: 1px solid ${neutrals.background_0};

            --tridactyl-url-fg: ${neutrals.foreground};
            --tridactyl-url-bg: ${neutrals.background_0};

            --tridactyl-of-fg: ${hues.green}
            --tridactyl-of-bg: ${neutrals.background_1};

            --tridactyl-hintspan-font-family: JetBrainsMonoNerdFont, monospace;
            --tridactyl-hintspan-font-size: 14px;
            --tridactyl-hintspan-font-weight: bold;
            --tridactyl-hintspan-fg: ${neutrals.foreground};
            --tridactyl-hintspan-bg: ${hues.blue};

            --tridactyl-hint-active-fg: ${neutrals.foreground};
            --tridactyl-hint-active-bg: ${hues.green}80;
            --tridactyl-hint-active-outline: 1px solid ${backgrounds.bg_green};

            --tridactyl-hint-bg: ${backgrounds.bg_blue}40;
            --tridactyl-hint-outline: 1px solid ${backgrounds.bg_blue};

            --tridactyl-highlight-box-bg: ${neutrals.background_0};
            --tridactyl-highlight-box-fg: ${neutrals.foreground};
        }
        :root .TridactylStatusIndicator {
          border-radius: 0px !important;
          padding: 8px !important;
          text-align: center !important;
          font-size: 14px !important;
          font-weight: bold !important;
          width: 70px !important;
        }
        :root.TridactylOwnNamespace a {
          color: ${hues.green};
        }
      '';
  };
}
