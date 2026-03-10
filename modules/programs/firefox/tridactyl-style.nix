{
  config,
  lib,
  ...
}:
with config.theme;
{
  config = lib.mkIf config.nx.programs.firefox.enable {
    home-manager.users.${config.nx.username}.home.file.".config/tridactyl/themes/customtheme.css".text =
      # css
      ''
        :root {
            --tridactyl-fg: ${foreground};
            --tridactyl-bg: ${bg0};

            --tridactyl-scrollbar-color: ${grey0} ${foreground};

            --tridactyl-cmdl-fg: ${foreground};
            --tridactyl-cmdl-bg: ${bg0};

            --tridactyl-header-first-bg: ${bg1};
            --tridactyl-header-second-bg: ${bg1};
            --tridactyl-header-third-bg: ${bg1};

            --tridactyl-cmplt-border-top: 1px solid ${bg0};

            --tridactyl-url-fg: ${foreground};
            --tridactyl-url-bg: ${bg0};

            --tridactyl-of-fg: ${green}
            --tridactyl-of-bg: ${bg1};

            --tridactyl-hintspan-font-family: JetBrainsMonoNerdFont, monospace;
            --tridactyl-hintspan-font-size: 14px;
            --tridactyl-hintspan-font-weight: bold;
            --tridactyl-hintspan-fg: ${foreground};
            --tridactyl-hintspan-bg: ${blue};

            --tridactyl-hint-active-fg: ${foreground};
            --tridactyl-hint-active-bg: ${green}80;
            --tridactyl-hint-active-outline: 1px solid ${bg_green};

            --tridactyl-hint-bg: ${bg_blue}40;
            --tridactyl-hint-outline: 1px solid ${bg_blue};

            --tridactyl-highlight-box-bg: ${bg0};
            --tridactyl-highlight-box-fg: ${foreground};
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
          color: ${green};
        }
      '';
  };
}
