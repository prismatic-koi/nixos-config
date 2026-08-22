{
  config,
  lib,
  ...
}:
with config.themev2;
# Colours source from config.themev2 (issue #2814). This file sets the
# --system-theme-* CSS variables injected into site stylesheets; the gap
# mappings match modules/programs/qutebrowser/greasemonkey.nix exactly so
# firefox and qutebrowser theme the same pages identically:
#   aqua        -> hues.teal            (v1 aqua == themev2 teal value)
#   grey0       -> neutrals.background_5 (structural grey)
#   grey1       -> neutrals.foreground_dim (dim text)
#   grey2       -> neutrals.foreground
#   statusline1 -> hues.indigo
#   statusline2 -> neutrals.foreground_dim
#   statusline3 -> hues.red
{
  config = lib.mkIf config.nx.programs.firefox.enable {
    home-manager.users.${config.nx.username}.programs.firefox.profiles.main.userContent =
      # css
      ''
        /* gmail checker extension */
        @-moz-document url-prefix("moz-extension://c99d673f-ecaa-4c0f-8dc7-f7da7a89e166") {
          .share-button {
            display: none !important
          }

        }

        /* squared google calendar */
        @-moz-document url-prefix("https://calendar.google.com") {
          * {
            border-radius: 0px !important;
            text-rendering: auto !important;
          }

          *:before {
            border-radius: 0px !important;
            text-rendering: auto !important;
          }

          *:after {
            border-radius: 0px !important;
            text-rendering: auto !important;
          }

          body {
            font-family: JetBrains Mono, monospace !important;
          }
        }
        @-moz-document url-prefix("https://www.metservice.com") {
          * {
            border-radius: 0px !important;
            text-rendering: auto !important;
            font-family: JetBrains Mono, monospace !important;
          }
          html {
            background-color: ${neutrals.background_1} !important;
          }
          .Container--blue,
          .App-container, .App-primary,
          .Nav-scroller,
          .Map-cartography,
          .sticky-headers,
          .sticky-bar-context-handler,
          .Nav--secondary .Container,
          .sticky-bar-context-primary-section {
            background-color: ${neutrals.background_0} !important;
          }
          .Nav-toggle,
          .Nav--primary {
            background-color: ${neutrals.background_1} !important;
          }
          #InitialPageLoader,
          .Main {
            background-color: ${neutrals.background_dim} !important;
          }
          #Nav,
          #Nav--primary,
          #Nav--secondary,
          #Container-inner,
          #Container--blue {
            background-color: ${neutrals.background_0} !important;
          }

          .Map .Map--primary .Map--static,
          .Nav--secondary .Container::before,
          .Nav--secondary .Container::after,
          .Container-inner.SearchContainer::before,
          .Container-inner.SearchContainer.SearchContainer--blue-solid::before,
          .Container-inner.SearchContainer.SearchContainer--blue-solid::after,
          .Container--secondary,
          .Container--blue--gradient {
            background: none !important;
          }
          div[data-module-name="favourite-summary-mod"] {
            display: none !important;
          }
          .SearchBar .domRef > .u-flex .u-flexAlignItemsCenter,
          .Footer-section--promo {
            display: none !important;
          }
          .Footer-section--global {
            background-color: ${neutrals.background_dim} !important;
          }
          .pageLoader-animation {
            background: ${neutrals.background_0} !important;
          }
          .Nav--secondary .Nav-menu-list-item.is-active a,
          .Nav-menu-list-item.is-active a {
            color: ${neutrals.background_0} !important;
            background-color: ${roles.primary} !important;
            box-shadow: none !important;
          }
          .Nav-menu-list-item a {
            color: ${neutrals.foreground} !important;
          }
          .SearchBar-actions-icon {
            color: ${roles.primary} !important;
          }
          .SearchBar-actions-find:focus,
          .SearchBar-actions-find:hover {
            background: none !important;
          }
          .SearchBar-actions-find {
            border-color: ${roles.primary} !important;
            box-shadow: none !important;
            color: ${neutrals.foreground} !important;
            margin-right: 0px !important;
          }
          .SearchBar {
            background-color: ${neutrals.background_0} !important;
            box-shadow: none !important;
          }

        }

        /* squared customised youtube */
        @-moz-document url-prefix("https://www.youtube.com") {
          * {
            border-radius: 0px !important;
            font-family: JetBrains Mono, monospace !important;
          }

          html[dark], [dark] {
            --yt-spec-base-background: ${neutrals.background_dim} !important;
            --yt-spec-raised-background: ${neutrals.background_0} !important;
            --yt-spec-menu-background: ${neutrals.background_0} !important;
            --yt-spec-static-overlay-text-primary: ${neutrals.foreground} !important;
            --yt-spec-static-overlay-text-secondary: ${neutrals.foreground} !important;
            --yt-spec-text-primary: ${neutrals.foreground} !important;
            --yt-spec-text-secondary: ${neutrals.foreground} !important;
            --ytd-searchbox-background: ${neutrals.background_dim} !important;
            --ytd-searchbox-legacy-border-color: ${neutrals.background_0} !important;
          }
          .yt-spec-button-shape-next--call-to-action.yt-spec-button-shape-next--text {
            color: ${hues.green} !important;
          }
          ytd-app {
            --ytd-mini-guide-width: 0px !important;
          }
          ytd-mini-guide-renderer {
            display: none !important;
          }
          #contents {
            margin-left: 0px !important;
          }
          /* hide sugested videos section */
          /* #secondary { */
          /*   display: none !important; */
          /* } */
          /* note, this will mess with the video format unless you apply a cookie like this `document.cookie = 'wide=1; expires='+new Date('3099').toUTCString()+'; path=/';` */
        }

        /* squared customised github */
        @-moz-document url-prefix("https://github.com") {
          * {
            border-radius: 0px !important;
          }

          body {
            font-family: JetBrains Mono, monospace !important;
          }
        }

        /* squared customised jira */
        @-moz-document url-prefix("https://jardengroup.atlassian.net") {
          * {
            border-radius: 0px !important;
          }

          body {
            font-family: JetBrains Mono, monospace !important;
          }
        }

        /* themed reddit */
        @-moz-document url-prefix("https://www.reddit.com") {
          :root {
            --system-theme-fg: ${neutrals.foreground};
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
            --system-theme-statusline1: ${hues.indigo};
            --system-theme-statusline2: ${neutrals.foreground_dim};
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

          /* Hide annoying stuff */
          .give-gold-button,
          .goldvertisement,
          .embed-comment,
          .premium-banner-outer,
          .sidebox,
          .account-activity-box,
          .share,
          .link-save-button,
          .hide-button,
          .report-button,
          .crosspost-button,
          .noCtrlF,
          .promoted,
          .nub {
            display: none !important;
          }

          * {
            font-family: JetBrains Mono, monospace !important;
            border-radius: 0px !important;
          }

          body {
            font-size: small !important;
          }

          /* main table */
          .sitetable {
            background-color: var(--system-theme-bg_dim) !important;
          }
          /* main table links */
          .res-nightmode .link, .res-nightmode .helpcenter-form .section-content {
            background-color: var(--system-theme-bg0) !important;
            border-color: var(--system-theme-bg_dim) !important;
            border-style: solid !important;
            border-width: 1px !important;
            padding: 6px !important;
            margin-top: 2px !important;
            margin-bottom: 2px !important;
            box-shadow: rgba(52, 63, 68, 0.25) 0px 1px 1px 0px, rgba(52, 63, 68, 0.31) 0px 0px 1px 0px;
            display: flex !important;
          }
          .res-nightmode .tagline, .res-nightmode .entry .buttons li a, .res-nightmode .trending-subreddits strong {
            color: var(--system-theme-grey2) !important;
          }
          /* page marker */
          .res-nightmode .RESDialogSmall > h3, .res-nightmode .NERPageMarker {
            background-color: var(--system-theme-bg0) !important;
          }
          .res-nightmode .RESDialogSmall > h3, .res-nightmode .NERPageMarker > a {
            color: var(--system-theme-red) !important;
            font-weight: bold !important;
          }

          /* post rank */
          .rank {
            font-size: large !important;
            text-align: center !important;
            width: 30px !important;
            color: var(--system-theme-green) !important;
            margin-top: auto !important;
            margin-bottom: auto !important;
            flex-shrink: 0 !important;
          }
          /* votes */
          .midcol {
            font-size: medium !important;
            width: 60px !important;
            flex-shrink: 0 !important;
            margin-top: auto !important;
            margin-bottom: auto !important;
            margin-left: 0px !important;
            margin-right: 0px !important;
          }
          .score.unvoted,
          .score.dislikes,
          .score.likes{
            color: var(--system-theme-fg) !important;
            flex-shrink: 0 !important;
          }
          .thumbnail {
            width: 70px !important;
            flex-shrink: 0 !important;
          }
          /* prevent post headings and subreddits from wrapping */
          .entry,
          .entry.unvoted,
          .entry.likes,
          .entry.dislikes {
            flex-grow: 1;
          }

          /* header area */
          #header-img {
            display: none !important;
          }
          .res-nightmode #sr-header-area, .res-nightmode #sr-more-link,
          .res-nightmode #RESShortcutsEditContainer, .res-nightmode #RESShortcutsSort, .res-nightmode #RESShortcutsRight, .res-nightmode #RESShortcutsLeft, .res-nightmode #RESShortcutsAdd {
            color: var(--system-theme-fg) !important;
            background-color: var(--system-theme-bg0) !important;
          }
          .res-nightmode #header, .res-nightmode .liveupdate-home .content {
            color: var(--system-theme-fg) !important;
            background-color: var(--system-theme-bg2) !important;
          }
          /* posts */
          .res-nightmode .entry.res-selected > .tagline, .res-nightmode .entry.res-selected .md-container > .md, .res-nightmode .entry.res-selected .md-container > .md p,
          .res-nightmode .entry.res-selected, .res-nightmode .entry.res-selected .md-container > .md {
            color: var(--system-theme-fg) !important;
          }
          .res-nightmode .res-hasNewComments .newComments {
            color: var(--system-theme-orange) !important;
          }
          /* comments */
          .usertext-body > .md {
            color: var(--system-theme-fg) !important;
          }
          .comment {
            background-color: var(--system-theme-bg1) !important;
          }
          .commentarea {
            color: var(--system-theme-fg) !important;
            background-color: var(--system-theme-bg1) !important;
          }
          .res-nightmode .comment .child:empty {
            display: none !important;
          }
          .res-nightmode .comment .child {
            background-color: var(--system-theme-bg1) !important;
            border-color: var(--system-theme-bg_dim) !important;
            border-width: 5px !important;
            border-style: solid !important;
          }
          .res-nightmode.res-commentBoxes .comment, .res-nightmode.res-commentBoxes .comment .comment .comment, .res-nightmode.res-commentBoxes .comment .comment .comment .comment .comment, .res-nightmode.res-commentBoxes .comment .comment .comment .comment .comment .comment .comment, .res-nightmode.res-commentBoxes .comment .comment .comment .comment .comment .comment .comment .comment .comment {
            color: var(--system-theme-fg) !important;
            border-radius: 0px !important;
            border-width: 3px !important;
            border-color: var(--system-theme-bg_dim) !important;
          }
          /* sidebar */
          .res-nightmode html.res-nightmode, .res-nightmode body, .res-nightmode body .content, .res-nightmode .modal-body, .res-nightmode .side, .res-nightmode .icon-menu a, .res-nightmode .side .leavemoderator, .res-nightmode .side .leavecontributor-button, .res-nightmode .side .titlebox, .res-nightmode .side .spacer .titlebox .redditname, .res-nightmode .side .titlebox .flairtoggle, .res-nightmode .side .usertext-body .md ol, .res-nightmode .side .usertext-body .md ol ol, .res-nightmode .side .usertext-body .md ol ol li, .res-nightmode .modactionlisting table *, .res-nightmode .side .recommend-box .rec-item, .res-nightmode .crosspost-preview, .res-nightmode .crosspost-thing-preview, .res-nightmode .admin_takedown, .res-nightmode .happening-now {
            color: var(--system-theme-fg) !important;
            background-color: var(--system-theme-bg_dim) !important;
          }
          .res-nightmode .sidebox .spacer, .res-nightmode .side .linkinfo,
          .res-nightmode .side .titlebox form.flairtoggle, .res-nightmode .trophy-area .content, .res-nightmode .side .md ol, .res-nightmode .side .md ul {
            color: var(--system-theme-fg) !important;
            background-color: var(--system-theme-bg1) !important;
          }
          /* Righthand side bar (pointless) */
          .listing-chooser-collapsed .listing-chooser,
          .listing-chooser li.selected,
          .listing-chooser-collapsed .grippy,
          .grippy:after {
            display: none !important;
          }
          /* Default links */
          a {
            color: var(--system-theme-blue) !important;
          }
          a:visited, a:hover {
            color: var(--system-theme-magenta) !important;
          }

          /* Inputs and buttons */
          input,
          textarea,
          .linkinfo .shortlink input,
          .new-comment .usertext-body,
          .morelink a,
          .morelink:hover a,
          .fancy-toggle-button a,
          .usertext button {
            background: var(--system-theme-bg0) !important;
            color: var(--system-theme-fg) !important;
            font-weight: normal !important;
          }

          /* Listing */
          .thing .title,
          .title:visited,
          .thing .title:hover {
            color: var(--system-theme-fg) !important;
          }

          .expando-button {
            filter: brightness(45%) contrast(180%);
            background-color: transparent !important;
          }

          .moderator,
          .green {
            color: var(--system-theme-green) !important;
          }

          .admin,
          .nsfw-stamp * {
            color: var(--system-theme-red) !important;
          }

          .buttons li {
            padding: 0 !important;
          }

          .buttons a {
            margin-right: 8px !important;
            color: var(--system-theme-fg) !important;
          }
          .pagename a,
          .trophy-name,
          .buttons a:hover,
          .buttons a:visited {
            color: var(--system-theme-fg) !important;
          }

          .pagename,
          .tabmenu li,
          .link .midcol,
          .buttons a,
          .subreddit {
            font-weight: normal !important;
          }

          .search-expando.collapsed:before,
          .comment-fade {
            display: none !important;
          }

          .recommended-link {
            border-color: var(--system-theme-bg0) !important;
          }

          /* Comments */
          .link .usertext .md,
          blockquote,
          pre,
          code,
          .md blockquote {
            border-left: solid 4px var(--system-theme-bg0) !important;
          }

          .md td {
            border: solid 1px var(--system-theme-bg0) !important;
          }

          hr {
            border-bottom: solid 1px var(--system-theme-bg0) !important;
          }

          .comment .author,
          .morecomments a {
            font-weight: normal !important;
          }

          /* RES */
          .guider,
          .guiders_button,
          .res-fancy-toggle-button,
          #RESConsoleContainer,
          #RESShortcutsAddFormContainer {
            background: var(--system-theme-bg0) !important;
          }

          .RESDialogSmall,
          .RESDropdownOptions,
          .RESNotification,
          #alert_message {
            background: var(--system-theme-bg0) !important;
          }

          .RES-keyNav-activeElement,
          .RES-keyNav-activeElement .md-container {
            color: var(--system-theme-fg) !important;
            background: var(--system-theme-bg0) !important;
          }

          .res-nightmode .arrow {
            filter: none !important;
          }

          .res-flairSearch.linkflairlabel>a {
            color: var(--system-theme-fg) !important;
          }

          /* Upvote/dowmnvote */
          .arrow.upmod {
            background-image: url(https://i.imgur.com/fmckYhF.png);
            /* Thick Arrow Pointing Up icon by Icons8: https://icons8.com/icon/99690/thick-arrow-pointing-up */
            background-position: -4.5px -1px;
            background-repeat: no-repeat;
            background-size: 24px 24px;
          }

          .arrow.downmod {
            background-image: url(https://i.imgur.com/aJJvTy5.png);
            /* Thick Arrow Pointing Up icon by Icons8: https://icons8.com/icon/99690/thick-arrow-pointing-up */
            background-position: -4.5px -9px;
            background-repeat: no-repeat;
            background-size: 24px 24px;
          }

          /* Score */
          .link .score.likes {
            color: var(--system-theme-red) !important;
          }

          .link .score.dislikes {
            color: var(--system-theme-magenta) !important;
          }

          .linkinfo>div:nth-child(2)>span:nth-child(1) {
            color: var(--system-theme-green) !important;
          }

          /* Subreddit header */
          .pagename a,
          .trophy-name {
            color: var(--system-theme-aqua) !important;
          }

          .tabmenu .selected a {
            color: var(--system-theme-aqua) !important;
            font-weight: bold;
          }

          /* Misc */
          a.edit-btn {
            background-color: var(--system-theme-fg) !important;
            white-space: nowrap;
            float: none;
          }

          .linkinfo .shortlink input,
          .new-comment .usertext-body,
          .morelink a,
          .morelink:hover a,
          .fancy-toggle-button a {
            background: var(--system-theme-bg0) !important;
            color: var(--system-theme-fg) !important;
            font-weight: normal !important;
          }

          .submit-text>div:nth-child(1)>a:nth-child(1),
          .submit-link>div:nth-child(1)>a:nth-child(1) {
            background: var(--system-theme-blue) !important;
            color: var(--system-theme-bg0) !important;
          }

          a.hover {
            color: var(--system-theme-aqua) !important;
          }
        }
        @-moz-document url-prefix("https://search.tinfoilforest.nz") {
          * {
            font-family: JetBrains Mono, monospace !important;
            border-radius: 0px !important;
          }
          :root {
            --color-base-font: ${neutrals.foreground} !important;
            --color-base-background: ${neutrals.background_0} !important;
            --color-base-background-mobile: ${neutrals.background_0} !important;
            --color-url-font: ${roles.primary} !important;
            --color-url-visited-font: ${hues.purple} !important;
            --color-header-background: ${neutrals.background_dim} !important;
            --color-header-border: ${neutrals.background_0} !important;
            --color-footer-background: ${neutrals.background_dim} !important;
            --color-footer-border: ${neutrals.background_0} !important;
            --color-sidebar-border: ${neutrals.background_1} !important;
            --color-sidebar-font: ${neutrals.foreground} !important;
            --color-sidebar-background: ${neutrals.background_0} !important;
            --color-backtotop-font: ${neutrals.foreground} !important;
            --color-backtotop-border: ${neutrals.background_0} !important;
            --color-backtotop-background: ${neutrals.background_dim} !important;
            --color-btn-background: ${roles.primary} !important;
            --color-btn-font: ${neutrals.background_0} !important;
            --color-show-btn-background: ${neutrals.background_1} !important;
            --color-show-btn-font: ${neutrals.foreground} !important;
            --color-search-border: ${neutrals.background_1} !important;
            --color-search-shadow: none !important;
            --color-search-background: ${neutrals.background_0} !important;
            --color-search-font: ${neutrals.foreground} !important;
            --color-search-background-hover: ${roles.primary} !important;
            --color-error: #f55b5b;
            --color-error-background: darken(#db3434, 40%);
            --color-warning: #f1d561;
            --color-warning-background: darken(#dbba34, 40%);
            --color-success: #79f56e;
            --color-success-background: darken(#42db34, 40%);
            --color-categories-item-selected-font: ${roles.primary} !important;
            --color-categories-item-border-selected: ${roles.primary} !important;
            --color-autocomplete-font: ${neutrals.foreground} !important;
            --color-autocomplete-border: ${neutrals.background_1} !important;
            --color-autocomplete-shadow: none !important;
            --color-autocomplete-background: ${neutrals.background_dim} !important;
            --color-autocomplete-background-hover: ${neutrals.background_dim} !important;
            --color-answer-font: ${neutrals.foreground} !important;
            --color-answer-background: ${neutrals.background_0} !important;
            --color-result-background: ${neutrals.background_0} !important;
            --color-result-border: ${neutrals.background_0} !important;
            --color-result-url-font: ${neutrals.foreground} !important;
            --color-result-vim-selected: #1f1f23cc;
            --color-result-vim-arrow: ${roles.primary} !important;
            --color-result-description-highlight-font: ${neutrals.foreground} !important;
            --color-result-link-font: ${roles.primary} !important;
            --color-result-link-font-highlight: ${roles.primary} !important;
            --color-result-link-visited-font: ${hues.purple} !important;
            --color-result-publishdate-font: ${neutrals.foreground} !important;
            --color-result-engines-font: ${neutrals.foreground} !important;
            --color-result-search-url-border: ${neutrals.background_1} !important;
            --color-result-search-url-font: ${neutrals.foreground} !important;
            --color-result-detail-font: ${neutrals.foreground} !important;
            --color-result-detail-label-font: lightgray;
            --color-result-detail-background: ${neutrals.background_0}
            --color-result-detail-hr: ${neutrals.background_1} !important;
            --color-result-detail-link: ${roles.primary} !important;
            --color-result-detail-loader-border: rgba(255, 255, 255, 0.2);
            --color-result-detail-loader-borderleft: rgba(0, 0, 0, 0);
            --color-result-image-span-font: ${neutrals.foreground} !important;
            --color-result-image-span-font-selected: ${neutrals.background_0} !important;
            --color-result-image-background: ${neutrals.background_0} !important;
            --color-settings-tr-hover: #2c2c32;
            --color-settings-engine-description-font: darken(#dcdcdc, 30%);
            --color-settings-table-group-background: #1b1b21;
            --color-toolkit-badge-font: ${neutrals.foreground} !important;
            --color-toolkit-badge-background: ${neutrals.background_1} !important;
            --color-toolkit-kbd-font: #000;
            --color-toolkit-kbd-background: ${neutrals.foreground} !important;
            --color-toolkit-dialog-border: ${neutrals.background_1} !important;
            --color-toolkit-dialog-background: ${neutrals.background_dim} !important;
            --color-toolkit-tabs-label-border: ${neutrals.background_0} !important;
            --color-toolkit-tabs-section-border: ${neutrals.background_1} !important;
            --color-toolkit-select-background: #313338;
            --color-toolkit-select-border: ${neutrals.background_1} !important;
            --color-toolkit-select-background-hover: #373b49;
            --color-toolkit-input-text-font: ${neutrals.foreground} !important;
            --color-toolkit-checkbox-onoff-off-background: #313338;
            --color-toolkit-checkbox-onoff-on-background: #313338;
            --color-toolkit-checkbox-onoff-on-mark-background: ${roles.primary} !important;
            --color-toolkit-checkbox-onoff-on-mark-color: ${neutrals.background_0} !important;
            --color-toolkit-checkbox-onoff-off-mark-background: #ddd;
            --color-toolkit-checkbox-onoff-off-mark-color: ${neutrals.background_0} !important;
            --color-toolkit-checkbox-label-background: ${neutrals.background_0} !important;
            --color-toolkit-checkbox-label-border: ${neutrals.background_0} !important;
            --color-toolkit-checkbox-input-border: ${roles.primary} !important;
            --color-toolkit-engine-tooltip-border: ${neutrals.background_0} !important;
            --color-toolkit-engine-tooltip-background: ${neutrals.background_0} !important;
            --color-toolkit-loader-border: rgba(255, 255, 255, 0.2);
            --color-toolkit-loader-borderleft: rgba(0, 0, 0, 0);
            --color-doc-code: #ddd;
            --color-doc-code-background: #4d5a6f;
          }
        }
      '';
  };
}
