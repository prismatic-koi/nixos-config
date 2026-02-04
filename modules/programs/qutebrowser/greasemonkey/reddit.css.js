// ==UserScript==
// @name         Userstyle (reddit.css)
// @match        https://www.reddit.com/*
// ==/UserScript==
GM_addStyle(`
  :root {
    --fontsize-small: 0.75rem;
    --fontsize-medium: 0.875rem;
    --fontsize-large: 1rem;

  }
  * { font: "Noto Sans", sans-serif !important;}
  body {
    font: normal x-small Noto Sans,arial,sans-serif;
    background-color: var(--system-theme-bg0) !important;
    color: var(--system-theme-fg) !important;
    padding-top: 40px !important;
  }
  .content { margin-left: 0px !important; border: none !important; background-color: var(--system-theme-bg_dim) !important; padding: 10px 0; }
  .side { background-color: var(--system-theme-bg_dim) !important; padding: 10px; box-sizing: border-box; border-color: var(--system-theme-bg2) !important; border-width: 1px; border-style: solid; border-radius: 5px; }
  .side .spacer { background-color: var(--system-theme-bg0); }
  .thing .title a.title { color: var(--system-theme-primary) !important; font-size: 1.125rem !important; font-weight: 600 !important; }
  .thing .title a.title:visited { color: var(--system-theme-grey0) !important; }
  .thing { background-color: var(--system-theme-bg0) !important; border-color: var(--system-theme-bg2) !important; border-width: 1px; border-style: solid; border-radius: 5px; margin-bottom: 10px !important; }
  .link .expand { background-color: var(--system-theme-bg2) !important; border: none !important; }
  .commentarea { background-color: var(--system-theme-bg0) !important; }
  .comment { border-color: var(--system-theme-bg2) !important; }
  .comment .author, .comment .submitter { color: var(--system-theme-blue) !important; }
  .md, .usertext-body { color: var(--system-theme-fg); }
  .md a, .usertext-body a { color: var(--system-theme-primary); }
  .md code, .md pre { font-family: "JetBrainsMono NF", monospace !important; background-color: var(--system-theme-bg2) !important; color: var(--system-theme-fg) !important; border-radius: 5px; padding: 4px; }
  #header, #sr-header-area, #header-bottom-left, .side #search, .searchexpando, #header-bottom-right, .listing-chooser, .expando-button { display: none !important; }
  .morelink { background-image: none !important; background-color: var(--system-theme-bg2) !important; color: var(--system-theme-primary) !important; border: 1px solid var(--system-theme-bg4) !important; border-radius: 5px;}
  .morelink .nub, .morelink:hover .nub, .mlhn { display: none !important; }
  .morelink a { color: var(--system-theme-primary) !important; }
  .morelink:hover a { color: var(--system-theme-fg) !important; }
  .nextprev { color: var(--system-theme-fg) !important; margin-left: 10px !important;}
  .nextprev a { background-color: var(--system-theme-bg2) !important; color: var(--system-theme-primary) !important; border: 1px solid var(--system-theme-bg4) !important; border-radius: 5px; }
  .footer-parent { display: none !important; }
  #hsts_pixel { display: none !important; }
  .debuginfo { display: none !important; }
  .flat-list buttons { color: var(--system-theme-grey2) !important;}

  /* --- Card Layout --- */
  .link.thing { background-color: var(--system-theme-bg0); border: 1px solid var(--system-theme-bg2); border-radius: 5px; margin: 0 10px 5px 10px; padding: 6px; box-sizing: border-box; }
  .link.thing .rank { padding: 10px !important; font-weight: bold !important; align-self: center !important; margin-top: 0 !important; padding 10px; color: var(--system-theme-fg); }
  .link.thing .midcol { padding-top: 0 !important; width: 30px; }
  .link .score { color: var(--system-theme-fg) !important; font-size: 1.1em; }
  .tagline { font-size: var(--fontsize-small) !important; color: var(--system-theme-fg) !important; }
  .tagline a, .search-result-meta a { color: var(--system-theme-primary) !important; }
  .flat-list.buttons { font-size: x-small !important; }
  .flairrichtext, .flair, .linkflairlabel { font: 500 12px "Noto Sans", sans-serif !important};
  .flaircolordark { color: var(--system-theme-bg0) !important;}
  .flair, .linkflairlabel { color: var(--system-theme-bg0) !important; background-color: var(--system-theme-orange) !important; border-color: var(--system-theme-bg0) !important}
  .link .rank { font-family: Noto Sans, sans-serif !important;}
  .thumbnail { margin-right: 10px !important; }

  /* --- post --- */
  .usertext-body, .link .usertext-body .md { font-size: var(--fontsize-large) !important; background-color: var(--system-theme-bg0) !important; border-color: var(--system-theme-blue); border-radius: 5px; }
  textarea { color: var(--system-theme-fg) !important; background-color: var(--system-theme-bg0) !important; border: 1px solid var(--system-theme-bg2) !important; border-radius: 5px; padding: 8px; font-size: var(--fontsize-large) !important; }
  .usertext-edit .md-container { background-color: var(--system-theme-bg0) !important; border: 1px solid var(--system-theme-blue) !important; border-radius: 5px; }
  .titlebox form.toggle, .leavemoderator { color: var(--system-theme-grey2) !important; background-color: var(--system-theme-bg2) !important; }
  .titlebox { padding: 5px !important;}
  .titlebox h1 { font-family: Noto Sans, sans-serif !important;}
  .titlebox h1 a { color: var(--system-theme-fg) !important;}
  .linkinfo { background-color: var(--system-theme-bg2) !important; border: 1px solid var(--system-theme-blue) !important; border-radius: 5px; padding: 8px; }
  .sidebox .subtitle { color: var(--system-theme-grey2) !important; font-size: bold !important; }
  a { color: var(--system-theme-primary) }
  .entry .buttons li a { color: var(--system-theme-grey2) !important; }
  .md blockquote, .md del { color: var(--system-theme-grey2) !important; }
  .dropdown.lightdrop .selected, .commentarea .menuarea { color: var(--system-theme-fg) !important;}
  .comment .child, .comment .showreplies {border-left: 1px solid var(--system-theme-bg2) !important;}
  .fancy-toggle-button .remove { background-image: none !important; background-color: var(--system-theme-bg2) !important; color: var(--system-theme-red) !important;}

  /* --- Header & Dropdowns --- */
  #custom-header-container { display: flex; justify-content: space-between; align-items: center; gap: 16px; padding: 5px 10px; background-color: var(--system-theme-bg1); border-bottom: 1px solid var(--system-theme-bg4); height: 40px; box-sizing: border-box; position: fixed; top: 0; left: 0; right: 0; z-index: 2000; }
  .header-left, .header-right { display: flex; align-items: center; gap: 16px; flex-shrink: 0; }
  .header-center { flex-grow: 1; display: flex; justify-content: center; }
  .custom-dropdown { position: relative; }
  .custom-dropdown-button { display: flex; justify-content: space-between; align-items: center; gap: 8px; background-color: var(--system-theme-bg2); color: var(--system-theme-fg); border: 1px solid var(--system-theme-bg4); border-radius: 5px; padding: 6px 12px; cursor: pointer; font-size: 12px; font-weight: bold; min-width: 150px; max-width: 220px; }
  .nav-dropdown > .custom-dropdown-button, .user-dropdown > .custom-dropdown-button, .sort-dropdown > .custom-dropdown-button { text-transform: none; }
  .custom-dropdown-button:hover { border-color: var(--system-theme-grey1); }
  .button-text { text-overflow: ellipsis; white-space: nowrap; overflow: hidden; }
  .notification-indicator { display: inline-block; width: 8px; height: 8px; background-color: var(--system-theme-red); border-radius: 50%; margin-left: 6px; }
  .custom-dropdown-content { display: none; position: absolute; background-color: var(--system-theme-bg1); min-width: 220px; box-shadow: 0px 8px 16px 0px rgba(0,0,0,0.2); z-index: 1001; border: 1px solid var(--system-theme-bg4); border-radius: 5px; margin-top: 4px; right: 0; padding: 4px 0; }
  .header-left .custom-dropdown-content { left: 0; right: auto; }
  .custom-dropdown.is-open .custom-dropdown-content { display: block; }
  .custom-dropdown-content > hr { margin: 6px 0; border: none; border-top: 1px solid var(--system-theme-bg2); }
  .main-button-chevron, .sub-toggle-chevron { display: flex; align-items: center; color: var(--system-theme-grey2); }
  .main-button-svg, .sub-toggle-svg { transition: transform 0.2s ease-in-out; transform: rotate(180deg); }
  .custom-dropdown.is-open .main-button-svg { transform: rotate(0deg); }
  .subheading-toggle-wrapper { display: flex; justify-content: space-between; align-items: center; cursor: pointer; padding: 8px 12px; }
  .subheading-toggle-wrapper:hover { background-color: var(--system-theme-bg2); }
  .dropdown-subheading { font-size: 11px; font-weight: bold; color: var(--system-theme-grey2); text-transform: uppercase; }
  .collapsible-section.is-expanded .sub-toggle-svg { transform: rotate(0deg); }
  .subreddit-list { display: none; max-height: 45vh; overflow-y: auto; scrollbar-width: none; -ms-overflow-style: none; }
  .subreddit-list::-webkit-scrollbar { display: none; }
  .collapsible-section.is-expanded .subreddit-list { display: block; }
  .menu-item { display: block; padding: 8px 12px; color: var(--system-theme-fg); text-decoration: none; font-size: 14px; white-space: nowrap; text-overflow: ellipsis; overflow: hidden; }
  .menu-item:hover { background-color: var(--system-theme-bg2); }
  .subreddit-list .menu-item { padding-left: 24px; }
  .user-menu-item { display: flex; align-items: center; gap: 10px; padding: 8px 12px; text-decoration: none; color: var(--system-theme-fg); font-size: 14px; cursor: pointer; }
  .user-menu-item:hover { background-color: var(--system-theme-bg2); }
  .user-menu-icon svg { width: 18px; height: 18px; stroke-width: 1.5px; flex-shrink: 0; color: var(--system-theme-grey2); }
  .user-dropdown .custom-dropdown-content { min-width: 200px; }
  .new-mail-icon, .new-notification-icon { color: var(--system-theme-orange) !important; }

  /* --- SEARCH STYLES --- */
  #custom-search-wrapper { max-width: 700px; width: 100%; position: relative; }
  #custom-search-wrapper > form { display: flex; align-items: center; width: 100%; background-color: var(--system-theme-bg0); border: 1px solid var(--system-theme-bg4); border-radius: 5px; }
  #custom-search-wrapper > form:focus-within { border-color: var(--system-theme-primary); }
  .search-icon { display: flex; align-items: center; color: var(--system-theme-grey2); padding-left: 10px; font-size: 1.4em; }
  .search-pill { display: flex; align-items: center; background-color: var(--system-theme-bg2); border-radius: 5px; padding: 2px 2px 2px 8px; margin-left: 6px; font-size: 13px; white-space: nowrap; }
  .pill-dismiss { display: flex; align-items: center; justify-content: center; border: none; background: var(--system-theme-bg4); color: var(--system-theme-fg); border-radius: 50%; width: 16px; height: 16px; font-size: 14px; line-height: 16px; margin-left: 6px; cursor: pointer; }
  .pill-dismiss:hover { background: var(--system-theme-grey1); }
  #custom-search-wrapper input[type="text"] { flex-grow: 1; border: none; outline: none; padding: 7px 8px; background: transparent; font-size: 14px; min-width: 100px; color: var(--system-theme-fg); }
  .clear-search-button { display: flex; align-items: center; justify-content: center; flex-shrink: 0; border: none; background: var(--system-theme-bg4); color: var(--system-theme-fg); border-radius: 50%; width: 16px; height: 16px; font-size: 14px; line-height: 16px; margin: 0 8px; cursor: pointer; }
  .clear-search-button:hover { background: var(--system-theme-grey1); }
  .search-options-popup { display: none; position: absolute; top: 105%; left: 0; width: 100%; background-color: var(--system-theme-bg1); border: 1px solid var(--system-theme-bg4); border-radius: 5px; padding: 8px 12px; box-shadow: 0 4px 8px rgba(0,0,0,0.2); z-index: 1000; white-space: nowrap; box-sizing: border-box; }
  #custom-search-wrapper > form:focus-within .search-options-popup { display: block; }
`);

// add cookie to force old reddit
document.cookie = 'redesign_optout=true; path=/; max-age=31536000';
