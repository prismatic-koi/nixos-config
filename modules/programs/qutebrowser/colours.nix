{
  config,
  lib,
  pkgs,
  ...
}:
{
  config = lib.mkIf config.nx.programs.qutebrowser.enable {
    home-manager.users.ben = {
      programs.qutebrowser.settings = {
        colors = with config.theme; {
          webpage = {
            preferred_color_scheme = type;
          };
          keyhint = {
            fg = foreground;
            suffix.fg = red;
            bg = bg0;
          };
          messages = {
            error = {
              bg = bg_red;
              fg = foreground;
            };
            info = {
              bg = bg_blue;
              fg = foreground;
            };
            warning = {
              bg = bg_yellow;
              fg = foreground;
            };
          };
          prompts = {
            bg = bg0;
            fg = foreground;
          };
          completion = {
            category = {
              bg = bg3;
              fg = foreground;
            };
            fg = foreground;
            even = {
              bg = bg0;
            };
            odd = {
              bg = bg_dim;
            };
            match = {
              fg = red;
            };
            item = {
              selected = {
                fg = foreground;
                bg = bg_yellow;
                border = {
                  top = bg_yellow;
                  bottom = bg_yellow;
                };
              };
            };
            scrollbar = {
              bg = bg_dim;
              fg = foreground;
            };
          };
          hints = {
            bg = bg0;
            fg = foreground;
            match = {
              fg = red;
            };
          };
          statusbar = {
            normal = {
              fg = foreground;
              bg = bg2;
            };
            insert = {
              fg = bg0;
              bg = statusline1;
            };
            caret = {
              fg = bg0;
              bg = purple;
            };
            command = {
              fg = foreground;
              bg = bg0;
            };
            passthrough = {
              fg = bg0;
              bg = blue;
            };
            url = {
              error = {
                fg = orange;
              };
              fg = foreground;
              hover = {
                fg = blue;
              };
              success = {
                http = {
                  fg = green;
                };
                https = {
                  fg = green;
                };
              };
            };
          };
          tabs = {
            bar = {
              bg = bg_dim;
            };
            even = {
              bg = bg0;
              fg = foreground;
            };
            odd = {
              bg = bg0;
              fg = foreground;
            };
            selected = {
              even = {
                bg = bg2;
                fg = foreground;
              };
              odd = {
                bg = bg2;
                fg = foreground;
              };
            };
            indicator = {
              start = blue;
              stop = green;
              error = red;
            };
          };
          downloads = {
            bar.bg = bg0;
            error = {
              bg = red;
              fg = bg0;
            };
            start = {
              bg = blue;
              fg = bg0;
            };
            stop = {
              bg = green;
              fg = bg0;
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
        #           background-color: ${config.theme.bg0};
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
