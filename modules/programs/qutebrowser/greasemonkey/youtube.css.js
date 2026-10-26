// ==UserScript==
// @name    Userstyle (youtube.css)
// @match        *://*.youtube.com/*
// @exclude     *://music.youtube.com/*
// ==/UserScript==
GM_addStyle(`
  * {
    border-radius: 5px !important;
    font-family: Noto Sans, sans-serif !important;
  }

  ytd-rich-grid-renderer {
    /* show more videos per row - Youtube used to look like this, but changed recently (2025-04-28) */
    --ytd-rich-grid-items-per-row: var(--ytd-rich-grid-slim-items-per-row) !important;
  }

  html, [dark], [light] {
    /* page background */
    --yt-sys-color-baseline--base-background: var(--system-theme-bg_dim) !important;
    --t3e41d7b17b187f69: var(--system-theme-bg_dim) !important;
    --yt-deprecated-general-background-a: var(--system-theme-bg_dim) !important;
    --yt-deprecated-general-background-b: var(--system-theme-bg_dim) !important;
    --yt-deprecated-general-background-c: var(--system-theme-bg_dim) !important;
    --yt-sys-color-baseline--frosted-glass-desktop: var(--system-theme-bg_dim) !important;
    --ta889dfda9605a358: var(--system-theme-bg_dim) !important;
    --yt-sys-color-baseline--frosted-glass-mobile: var(--system-theme-bg_dim) !important;
    --t5978da8d584b8fe9: var(--system-theme-bg_dim) !important;
    --yt-sys-color-baseline--frost-container-medium: var(--system-theme-bg_dim) !important;
    --tc54fd20dab7048eb: var(--system-theme-bg_dim) !important;
    --yt-sys-color-baseline--frost-container-semi-heavy: var(--system-theme-bg_dim) !important;
    --t25a42c37f53e50e0: var(--system-theme-bg_dim) !important;
    --yt-sys-color-baseline--frost-container-heavy: var(--system-theme-bg_dim) !important;
    --t5355e7dcd3a33a2f: var(--system-theme-bg_dim) !important;
    --yt-sys-color-baseline--themed-overlay-background: var(--system-theme-bg_dim) !important;
    --t0ccd1ace000d5e93: var(--system-theme-bg_dim) !important;
    /* raised, menu and container surfaces */
    --yt-sys-color-baseline--raised-background: var(--system-theme-bg0) !important;
    --t518e925f61bdcb91: var(--system-theme-bg0) !important;
    --yt-sys-color-baseline--menu-background: var(--system-theme-bg0) !important;
    --t08a7c6c176cbc5c2: var(--system-theme-bg0) !important;
    --yt-sys-color-baseline--surface-container: var(--system-theme-bg0) !important;
    --tbdeaf5bc17152abc: var(--system-theme-bg0) !important;
    --paper-menu-background-color: var(--system-theme-bg0) !important;
    --paper-listbox-background-color: var(--system-theme-bg0) !important;
    --paper-dialog-background-color: var(--system-theme-bg0) !important;
    --primary-background-color: var(--system-theme-bg0) !important;
    /* text */
    --yt-sys-color-baseline--text-primary: var(--system-theme-fg) !important;
    --yt-sys-color-baseline--inverted-background: var(--system-theme-fg) !important;
    --t1405e70a39276293: var(--system-theme-fg) !important;
    --tffc2fd3a644f6275: var(--system-theme-fg) !important;
    --yt-sys-color-baseline--text-secondary: var(--system-theme-grey2) !important;
    --t4a6da19e16bf221a: var(--system-theme-grey2) !important;
    --yt-sys-color-baseline--text-disabled: var(--system-theme-grey2) !important;
    --t7e34d5baa4ea6277: var(--system-theme-grey2) !important;
    --yt-sys-color-baseline--text-primary-inverse: var(--system-theme-bg_dim) !important;
    --t6216186c28b3834b: var(--system-theme-bg_dim) !important;
    --paper-menu-color: var(--system-theme-fg) !important;
    --paper-menu-disabled-color: var(--system-theme-fg) !important;
    --paper-listbox-color: var(--system-theme-fg) !important;
    --primary-text-color: var(--system-theme-fg) !important;
    --secondary-text-color: var(--system-theme-grey2) !important;
    --disabled-text-color: var(--system-theme-grey2) !important;
    --text-primary-color: var(--system-theme-bg_dim) !important;
    /* icons and wordmark */
    --yt-sys-color-baseline--wordmark-text: var(--system-theme-fg) !important;
    --yt-sys-color-baseline--system-bar: var(--system-theme-fg) !important;
    --yt-sys-color-baseline--touch-response: var(--system-theme-fg) !important;
    --tde41338fc2bd4ba5: var(--system-theme-fg) !important;
    --t4fb0f67b251e1a42: var(--system-theme-fg) !important;
    --t7f9b7e1603e20b94: var(--system-theme-fg) !important;
    --yt-sys-color-baseline--mono-filled-hover: var(--system-theme-fg) !important;
    --t8cdc9846a76caf1f: var(--system-theme-fg) !important;
    /* search box */
    --ytd-searchbox-background: var(--system-theme-bg0) !important;
    --ytd-searchbox-text-color: var(--system-theme-fg) !important;
    --ytd-searchbox-border-color: var(--system-theme-bg3) !important;
    --ytd-searchbox-legacy-border-color: var(--system-theme-bg3) !important;
    --ytd-searchbox-legacy-button-color: var(--system-theme-bg1) !important;
    --ytd-searchbox-legacy-button-hover-color: var(--system-theme-bg2) !important;
    --ytd-searchbox-legacy-button-focus-color: var(--system-theme-bg2) !important;
    --ytd-searchbox-legacy-button-border-color: var(--system-theme-bg3) !important;
    --ytd-searchbox-legacy-button-hover-border-color: var(--system-theme-bg3) !important;
    --ytd-searchbox-legacy-button-icon-color: var(--system-theme-fg) !important;
    --ytd-searchbox-legacy-border-shadow-color: var(--system-theme-bg1) !important;
    /* chips, buttons and dividers */
    --yt-sys-color-baseline--tonal-background: var(--system-theme-bg1) !important;
    --tf5f050c65aebead5: var(--system-theme-bg1) !important;
    --tc87f6a7f05e65fc4: var(--system-theme-bg1) !important;
    --yt-sys-color-baseline--additive-background: var(--system-theme-bg1) !important;
    --t7c4965c11f8537c0: var(--system-theme-bg1) !important;
    --t7f4f2c6d54836ce0: var(--system-theme-bg1) !important;
    --yt-sys-color-baseline--state-mono-standard-hovered: var(--system-theme-bg1) !important;
    --t1aab759cd9ddbe39: var(--system-theme-bg1) !important;
    --yt-sys-color-baseline--button-chip-background-hover: var(--system-theme-bg3) !important;
    --t20480717de80f555: var(--system-theme-bg3) !important;
    --t416e5931fc464589: var(--system-theme-bg3) !important;
    --yt-sys-color-baseline--outline: var(--system-theme-bg3) !important;
    --yt-sys-color-baseline--outline-opaque: var(--system-theme-bg3) !important;
    --td8562cdc203bc683: var(--system-theme-bg3) !important;
    --divider-color: var(--system-theme-bg3) !important;
    --yt-sys-color-baseline--suggested-action: var(--system-theme-bg_blue) !important;
    --tfa3475c508f5dfef: var(--system-theme-bg_blue) !important;
    /* links and call to action */
    --yt-sys-color-baseline--call-to-action: var(--system-theme-green) !important;
    --t2d807bb79e75606d: var(--system-theme-green) !important;
    --yt-sys-color-baseline--call-to-action-inverse: var(--system-theme-green) !important;
    --tb628117fc164ad87: var(--system-theme-green) !important;
    --yt-sys-color-baseline--call-to-action-hover: var(--system-theme-green) !important;
    --t617db776af0de196: var(--system-theme-green) !important;
    --primary-color: var(--system-theme-primary) !important;
    /* status colours */
    --yt-sys-color-baseline--static-brand-red: var(--system-theme-red) !important;
    --yt-sys-color-baseline--red-indicator: var(--system-theme-red) !important;
    --yt-sys-color-baseline--error-indicator: var(--system-theme-red) !important;
    --t854c79338d8ca8a7: var(--system-theme-red) !important;
    --yt-sys-color-baseline--brand-red-contrast: var(--system-theme-red) !important;
    --t8533f30445e4a126: var(--system-theme-red) !important;
    --td545df04e2c659d7: var(--system-theme-red) !important;
    --yt-sys-color-baseline--error-indicator-inverse: var(--system-theme-red) !important;
    --t53991d599b1488f5: var(--system-theme-red) !important;
    --yt-sys-color-baseline--text-error: var(--system-theme-red) !important;
    --t5208fd177a788cfa: var(--system-theme-red) !important;
    --error-color: var(--system-theme-red) !important;
    --yt-sys-color-baseline--text-warning: var(--system-theme-yellow) !important;
    --t3f3bdb4140d3ead7: var(--system-theme-yellow) !important;
    --yt-sys-color-baseline--icon-warning: var(--system-theme-yellow) !important;
    --tecbdf924b63b27fc: var(--system-theme-yellow) !important;
    --yt-sys-color-baseline--background-warning: var(--system-theme-bg_yellow) !important;
    --tf0a0d1154827847f: var(--system-theme-bg_yellow) !important;
    --yt-sys-color-baseline--text-complete: var(--system-theme-green) !important;
    --t4d7776c28db21122: var(--system-theme-green) !important;
    --yt-sys-color-baseline--themed-green: var(--system-theme-green) !important;
    --t904a88c623ca27ab: var(--system-theme-green) !important;
    --yt-sys-color-baseline--background-complete: var(--system-theme-bg_green) !important;
    --t4726c68ef6d7b08f: var(--system-theme-bg_green) !important;
  }

  /* YouTube sets per-video palettes in inline styles. The same custom
     properties are overridden here, on the elements that carry them. */
  ytd-watch-metadata {
    --yt-saturated-base-background: var(--system-theme-bg_dim) !important;
    --yt-saturated-raised-background: var(--system-theme-bg0) !important;
    --yt-saturated-additive-background: var(--system-theme-bg1) !important;
    --yt-saturated-text-primary: var(--system-theme-fg) !important;
    --yt-saturated-text-secondary: var(--system-theme-grey2) !important;
    --yt-saturated-text-disabled: var(--system-theme-grey2) !important;
    --yt-saturated-outline: var(--system-theme-bg3) !important;
    --yt-saturated-card-outline: var(--system-theme-bg3) !important;
    --yt-saturated-inverted-background: var(--system-theme-fg) !important;
    --yt-saturated-text-primary-inverse: var(--system-theme-bg_dim) !important;
  }
  ytd-expandable-metadata-renderer {
    --yt-lightsource-section1-color: var(--system-theme-bg1) !important;
    --yt-lightsource-section2-color: var(--system-theme-bg1) !important;
    --yt-lightsource-section3-color: var(--system-theme-bg1) !important;
    --yt-lightsource-section4-color: var(--system-theme-bg1) !important;
    --yt-lightsource-primary-title-color: var(--system-theme-fg) !important;
    --yt-lightsource-secondary-title-color: var(--system-theme-grey2) !important;
    --yt-lightsource-icon-activated-color: var(--system-theme-fg) !important;
    --yt-lightsource-icon-inactive-color: var(--system-theme-grey2) !important;
    --yt-lightsource-icon-disabled-color: var(--system-theme-grey2) !important;
  }
  ytd-macro-markers-list-item-renderer {
    --ytd-macro-markers-list-item-background-color: var(--system-theme-bg1) !important;
    --ytd-macro-markers-list-item-title-color: var(--system-theme-fg) !important;
    --ytd-macro-markers-list-item-secondary-color: var(--system-theme-grey2) !important;
    --ytd-macro-markers-list-item-timestamp-background-color: var(--system-theme-bg2) !important;
  }
  /* inline colours that YouTube sets on description text */
  .ytAttributedStringLinkInheritColor {
    color: inherit !important;
  }
  /* body links: the theme hue has too little contrast on a light theme */
  .ytAttributedStringLinkCallToActionColor,
  ytd-text-inline-expander a,
  ytd-watch-info-text a {
    color: var(--system-theme-fg) !important;
    text-decoration: underline !important;
  }
  /* calendar tile of the ticket shelf has a literal white background */
  #calendar-avatar-day.ytd-ticket-shelf-renderer {
    background-color: var(--system-theme-bg0) !important;
  }
  #calendar-avatar-day-text.ytd-ticket-shelf-renderer {
    color: var(--system-theme-fg) !important;
  }

  .yt-spec-button-shape-next--mono.yt-spec-button-shape-next--filled,
  .yt-spec-icon-badge-shape--style-overlay .yt-spec-icon-badge-shape__icon,
  .ytSearchboxComponentSearchButton,
  .ytSearchboxComponentInputBox,
  .ytSearchboxComponentInputBoxDark,
  .yt-spec-button-shape-next--overlay.yt-spec-button-shape-next--text {
    color: var(--system-theme-fg) !important;
    background-color: var(--system-theme-bg0) !important;
  }
  .ytSearchboxComponentInputBox,
  .ytSearchboxComponentInputBoxDark,
  .ytSearchboxComponentSearchButton,
  .ytSearchboxComponentSearchButtonDark {
    border: 1px solid var(--system-theme-grey2) !important;
    border-color: var(--system-theme-grey2) !important;
  }

  .ytSearchboxComponentInputBox, .ytSearchboxComponentInputBoxDark {
    border-radius: 5px 0 0 5px !important;
  }
  .ytSearchboxComponentSearchButton {
    border-radius: 0 5px 5px 0 !important;
  }

  /* weird black bands around a "cinimatic video" */
  .ytd-watch-flexy[full-bleed-player] #full-bleed-container.ytd-watch-flexy {
    background-color: var(--system-theme-bg_dim) !important;
  }

  /* progress bar */
  .ytp-play-progress.ytp-swatch-background-color {
    background: var(--system-theme-red) !important;
  }

  /* hide anoying branding over the top of the video bottom right */
  .annotation.annotation-type-custom.iv-branding {
    display: none !important;
  }

  .yt-spec-button-shape-next--call-to-action.yt-spec-button-shape-next--text {
    color: var(--system-theme-green) !important;
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
  /*
  #related {
    display: none !important;
  }
  */
  /* note, the above requires the wide=1 cookie to be set */
`)
// add cookie to force wide mode
document.cookie = 'wide=1; expires=' + new Date('3099').toUTCString() + ';domain=.youtube.com;path=/';

// just for fun, use my colourscheme for the red youtube logo
const observer = new MutationObserver(() => {
  const pathElement = document.querySelector('[id^="yt-ringo2-red-svg_yt"] > g:nth-child(1) > path');
  if (pathElement) {
    pathElement.setAttribute('fill', 'var(--system-theme-primary)');
  }
});

observer.observe(document.body, { childList: true, subtree: true });
