{
  config,
  lib,
  pkgs,
  ...
}:
{
  config = lib.mkIf config.nx.programs.qutebrowser.enable {
    home-manager.users.${config.nx.username} = {
      programs.qutebrowser.settings = {
        colors = with config.theme; {
          webpage = {
            preferred_color_scheme = type;
          };
          keyhint = {
            fg = neutrals.foreground;
            suffix.fg = hues.red;
            bg = neutrals.background_0;
          };
          messages = {
            error = {
              bg = backgrounds.bg_red;
              fg = neutrals.foreground;
            };
            info = {
              bg = backgrounds.bg_blue;
              fg = neutrals.foreground;
            };
            warning = {
              bg = backgrounds.bg_yellow;
              fg = neutrals.foreground;
            };
          };
          prompts = {
            bg = neutrals.background_0;
            fg = neutrals.foreground;
          };
          completion = {
            category = {
              bg = neutrals.background_3;
              fg = neutrals.foreground;
            };
            fg = neutrals.foreground;
            even = {
              bg = neutrals.background_0;
            };
            odd = {
              bg = neutrals.background_dim;
            };
            match = {
              fg = hues.red;
            };
            item = {
              selected = {
                fg = neutrals.foreground;
                bg = backgrounds.bg_yellow;
                border = {
                  top = backgrounds.bg_yellow;
                  bottom = backgrounds.bg_yellow;
                };
              };
            };
            scrollbar = {
              bg = neutrals.background_dim;
              fg = neutrals.foreground;
            };
          };
          hints = {
            bg = neutrals.background_0;
            fg = neutrals.foreground;
            match = {
              fg = hues.red;
            };
          };
          statusbar = {
            normal = {
              fg = neutrals.foreground;
              bg = neutrals.background_2;
            };
            insert = {
              fg = neutrals.background_0;
              # statusline1 has no theme equivalent (dropped in the migration).
              # Chosen accent for the insert/active-mode indicator: hues.indigo.
              bg = hues.indigo;
            };
            caret = {
              fg = neutrals.background_0;
              bg = hues.purple;
            };
            command = {
              fg = neutrals.foreground;
              bg = neutrals.background_0;
            };
            passthrough = {
              fg = neutrals.background_0;
              bg = hues.blue;
            };
            url = {
              error = {
                fg = hues.orange;
              };
              fg = neutrals.foreground;
              hover = {
                fg = hues.blue;
              };
              success = {
                http = {
                  fg = hues.green;
                };
                https = {
                  fg = hues.green;
                };
              };
            };
          };
          tabs = {
            bar = {
              bg = neutrals.background_dim;
            };
            even = {
              bg = neutrals.background_0;
              fg = neutrals.foreground;
            };
            odd = {
              bg = neutrals.background_0;
              fg = neutrals.foreground;
            };
            selected = {
              even = {
                bg = neutrals.background_2;
                fg = neutrals.foreground;
              };
              odd = {
                bg = neutrals.background_2;
                fg = neutrals.foreground;
              };
            };
            indicator = {
              start = hues.blue;
              stop = hues.green;
              error = hues.red;
            };
          };
          downloads = {
            bar.bg = neutrals.background_0;
            error = {
              bg = hues.red;
              fg = neutrals.background_0;
            };
            start = {
              bg = hues.blue;
              fg = neutrals.background_0;
            };
            stop = {
              bg = hues.green;
              fg = neutrals.background_0;
            };
            system = {
              bg = "rgb";
              fg = "rgb";
            };
          };
        };
        hints.border = "0px solid black";
        # content.user_stylesheets = let
        #   css = pkgs.writeTextFile {
        #     name = "qutebrowser.css";
        #     text =
        #       /*
        #       css
        #       */
        #       ''
        #         /* try to prevent a flash of unstyled content */
        #         body,
        #         /* reddit things */
        #         .premium-banner,
        #         .infobar.welcome h1,
        #         .button .cover,
        #         .commentsignupbar__container,
        #         .link.promotedlink.promoted,
        #         .link.promotedlink.external,
        #         .tabmenu li a,
        #         .side,
        #         body.with-listing-chooser.listing-chooser-collapsed .listing-chooser .grippy,
        #         #header, #sr-header-area, #header-bottom-left, .side #search, .searchexpando, #header-bottom-right, .listing-chooser, .expando-button
        #         {
        #           background-color: ${config.theme.neutrals.background_0};
        #         }
        #         /* hide other stuff */
        #         .subreddit-list {
        #           display: none;
        #         }
        #
        #       '';
        #   };
        # in ["${css}"];
      };
    };
  };
}
