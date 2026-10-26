{
  config,
  pkgs,
  lib,
  ...
}:
let
  # The qute://start logo is an external <img>, so page CSS in
  # startpage.css.js cannot reach the shapes inside it. Recolour a copy of
  # the upstream SVG at eval time, using the live theme palette, and apply
  # it with `content:` on `.logo` in startpage.css.js.
  #
  # Read from the qutebrowser package output rather than a vendored copy, so
  # this cannot drift from the upstream asset. If upstream moves or
  # restructures the icon, `builtins.readFile` throws and eval fails loudly
  # rather than silently falling back to an unthemed logo.
  qutebrowserLogoSvgPath = "${pkgs.qutebrowser}/${pkgs.python3.sitePackages}/qutebrowser/icons/qutebrowser.svg";
  qutebrowserLogoSvgRaw = builtins.readFile qutebrowserLogoSvgPath;

  qutebrowserLogoSvgThemed =
    let
      recoloured =
        builtins.replaceStrings
          [ "#cee5fd" "#7ebaff" "#0a396e" ]
          [
            config.theme.hues.blue
            config.theme.hues.green
            config.theme.neutrals.background_0
          ]
          qutebrowserLogoSvgRaw;
      # The upstream SVG is pretty-printed across ~100 lines. Embedded
      # verbatim, those raw newlines land inside a double-quoted CSS
      # `url("...")` string, which is a CSS parse error (a quoted string
      # cannot contain a literal newline) and silently drops the whole
      # `content` declaration. Collapse them to spaces before encoding.
      singleLine = builtins.replaceStrings [ "\n" "\r" ] [ " " " " ] recoloured;
    in
    # `#` starts a fragment inside a data URI and truncates it there, and
    # `"` would terminate the CSS string this URI is embedded in, so both
    # must be percent-encoded, along with the theme's substituted colours.
    builtins.replaceStrings [ "#" "\"" ] [ "%23" "%22" ] singleLine;
in
{
  config = lib.mkIf config.nx.programs.qutebrowser.enable {
    home-manager.users.${config.nx.username} = {
      programs.qutebrowser.greasemonkey = with config.theme; [
        # css styling for the qute://start logo, themed at eval time from
        # config.theme — see qutebrowserLogoSvgThemed above
        (pkgs.writeText "startpage-logo.css.js"
          # css
          ''
            // ==UserScript==
            // @name    Userstyle (startpage-logo.css)
            // @include   /^qute://start/*/
            // @include    about:blank
            // ==/UserScript==
            GM_addStyle(`
            .logo {
              content: url("data:image/svg+xml,${qutebrowserLogoSvgThemed}");
            }
            `)
          ''
        )
        # select the Home Assistant frontend theme named after config.theme.
        # HA stores {"theme": name, "dark"?, ...} in localStorage.selectedTheme.
        # Only `theme` is written; other stored keys are kept. `dark` stays
        # unset: each HA theme (prismatic-koi/hass-config themes/) defines only
        # the mode matching its type, so HA picks the mode itself.
        #
        # HA (frontend PR #28965) keeps the theme in the user profile, and the
        # profile value replaces localStorage after connect. So after connect
        # the script reads the profile value and, if it differs, fires the
        # frontend's own `settheme` event on <home-assistant>, which saves it
        # to the profile. The profile applies to ALL devices of the user; the
        # user accepts this (issue #3260). At most one dispatch per page load.
        (pkgs.writeText "home-assistant-theme.js"
          # javascript
          ''
            // ==UserScript==
            // @name    Home Assistant theme
            // @match   https://home-assistant.tinfoilforest.nz/*
            // @run-at  document-start
            // ==/UserScript==
            (() => {
              const name = ${builtins.toJSON name};
              try {
                let stored = {};
                try {
                  const parsed = JSON.parse(localStorage.getItem("selectedTheme"));
                  if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
                    stored = parsed;
                  }
                } catch (e) {}
                if (stored.theme !== name) {
                  localStorage.setItem("selectedTheme", JSON.stringify({ ...stored, theme: name }));
                }
              } catch (e) {}

              let done = false;
              const started = Date.now();
              const timer = setInterval(() => {
                try {
                  if (done || Date.now() - started > 30000) {
                    clearInterval(timer);
                    return;
                  }
                  const el = document.querySelector("home-assistant");
                  const conn = el && el.hass && el.hass.connection;
                  if (!conn) return;
                  done = true;
                  clearInterval(timer);
                  Promise.resolve(conn.sendMessagePromise({ type: "frontend/get_user_data", key: "theme" }))
                    .then((res) => {
                      const value = res && res.value;
                      if (value && value.theme === name) return;
                      el.dispatchEvent(
                        new CustomEvent("settheme", { detail: { theme: name }, bubbles: true, composed: true }),
                      );
                    })
                    .catch(() => {});
                } catch (e) {
                  clearInterval(timer);
                }
              }, 250);
            })();
          ''
        )
        # general theme variables, to be used in other scripts
        # made available here to all sites
        (pkgs.writeText "theme.css.js"
          # css
          ''
            // ==UserScript==
            // @name    Userstyle (theme.css)
            // @include   *
            // ==/UserScript==
            GM_addStyle(`
            :root {
              --system-theme-fg: ${neutrals.foreground};
              --system-theme-primary: ${roles.primary};
              --system-theme-secondary: ${roles.secondary};
              --system-theme-red: ${hues.red};
              --system-theme-orange: ${hues.orange};
              --system-theme-yellow: ${hues.yellow};
              --system-theme-green: ${hues.green};
              --system-theme-aqua: ${hues.teal};
              --system-theme-blue: ${hues.blue};
              --system-theme-purple: ${hues.purple};
              --system-theme-grey0: ${neutrals.background_5};
              --system-theme-grey1: ${neutrals.foreground_dim};
              --system-theme-grey2: ${neutrals.foreground};
              /* statusline1/2/3 have no theme equivalent (dropped in the
                 migration). A statusline slot carries status, so each maps to a
                 hue, never a grey: statusline1 -> hues.indigo (matches the
                 qutebrowser insert-mode statusbar choice in colours.nix),
                 statusline2 -> hues.yellow (warm; v1 gold #df8e1d == theme
                 yellow on latte), statusline3 -> hues.red. */
              --system-theme-statusline1: ${hues.indigo};
              --system-theme-statusline2: ${hues.yellow};
              --system-theme-statusline3: ${hues.red};
              --system-theme-bg_dim: ${neutrals.background_dim};
              --system-theme-bg0: ${neutrals.background_0};
              --system-theme-bg1: ${neutrals.background_1};
              --system-theme-bg2: ${neutrals.background_2};
              --system-theme-bg3: ${neutrals.background_3};
              --system-theme-bg4: ${neutrals.background_4};
              --system-theme-bg5: ${neutrals.background_5};
              --system-theme-bg_visual: ${backgrounds.bg_visual};
              --system-theme-bg_red: ${backgrounds.bg_red};
              --system-theme-bg_green: ${backgrounds.bg_green};
              --system-theme-bg_blue: ${backgrounds.bg_blue};
              --system-theme-bg_yellow: ${backgrounds.bg_yellow};
            }
            `)
          ''
        )
        # css styling for qutebrowser startpage
        (pkgs.writeTextFile {
          name = "startpage.css.js";
          text = builtins.readFile ./greasemonkey/startpage.css.js;
        })
        # sponsorblock for youtube videos
        (pkgs.writeTextFile {
          name = "youtube_sponsorblock.js";
          text = builtins.readFile ./greasemonkey/youtube_sponsorblock.js;
        })
        # dearrow for youtube titles
        # (pkgs.writeTextFile {
        #   name = "youtube_dearrow.js";
        #   text = builtins.readFile ./greasemonkey/youtube_dearrow.js;
        # })
        # some css styling for youtube
        (pkgs.writeTextFile {
          name = "youtube.css.js";
          text = builtins.readFile ./greasemonkey/youtube.css.js;
        })
        # adblock for reddit
        (pkgs.writeTextFile {
          name = "reddit_adblock.js";
          text = builtins.readFile ./greasemonkey/reddit_adblock.js;
        })
        # style for github.com
        (pkgs.writeTextFile {
          name = "github.css.js";
          text = builtins.readFile ./greasemonkey/github.css.js;
        })
        # style for wikipedia
        (pkgs.writeTextFile {
          name = "wikipedia.css.js";
          text = builtins.readFile ./greasemonkey/wikipedia.css.js;
        })
        # style for google calendar
        (pkgs.writeTextFile {
          name = "googlecalendar.css.js";
          text = builtins.readFile ./greasemonkey/googlecalendar.css.js;
        })
        # style for the proton web suite (mail, calendar, drive, account, pass)
        (pkgs.writeTextFile {
          name = "proton.css.js";
          text = builtins.readFile ./greasemonkey/proton.css.js;
        })
        # style for searx
        (pkgs.writeTextFile {
          name = "searx.css.js";
          text = builtins.readFile ./greasemonkey/searx.css.js;
        })
        # style for metservice.com
        (pkgs.writeTextFile {
          name = "metservice.css.js";
          text = builtins.readFile ./greasemonkey/metservice.css.js;
        })
        # style for reddit
        (pkgs.writeTextFile {
          name = "reddit.css.js";
          text = builtins.readFile ./greasemonkey/reddit.css.js;
        })
        (pkgs.writeTextFile {
          name = "reddit_custom_header.js";
          text = builtins.readFile ./greasemonkey/reddit_custom_header.js;
        })
        (pkgs.writeTextFile {
          name = "reddit_custom_footernix-pr-tracker.js";
          text = builtins.readFile ./greasemonkey/nix-pr-tracker.js;
        })
        # universal URL rewriter for fixing broken CDNs and timeouts
        (pkgs.writeTextFile {
          name = "url_rewriter.js";
          text = builtins.readFile ./greasemonkey/url_rewriter.js;
        })
        # restore background-color for sites broken by userstyle
        # (pkgs.writeTextFile {
        #   name = "background_restore.css.js";
        #   text = builtins.readFile ./greasemonkey/background_restore.css.js;
        # })
        # style for facebook
        # (pkgs.writeTextFile {
        #   name = "facebook.css.js";
        #   text = builtins.readFile ./greasemonkey/facebook.css.js;
        # })
        # delay load for reddit
        # interferes with reddit adblock, TODO: fix
        # (pkgs.writeTextFile {
        #   name = "reddit_delay_load.js";
        #   text = builtins.readFile ./greasemonkey/reddit_delay_load.js;
        # })
      ];
    };
  };
}
