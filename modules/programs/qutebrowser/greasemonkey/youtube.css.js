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

  html[dark], [dark],
  html[darker-dark], [darker-dark]
  html[darker-dark-theme-deprecate], [darker-dark-theme-deprecate] {
    --yt-spec-base-background: var(--system-theme-bg_dim) !important;
    --yt-spec-frosted-glass-desktop: var(--system-theme-bg_dim) !important;
    --yt-frosted-glass-desktop: var(--system-theme-bg_dim) !important;
    --yt-spec-raised-background: var(--system-theme-bg0) !important;
    --yt-spec-menu-background: var(--system-theme-bg0) !important;
    --yt-spec-static-overlay-text-primary: var(--system-theme-fg) !important;
    --yt-spec-static-overlay-text-secondary: var(--system-theme-grey2) !important;
    --yt-spec-text-primary: var(--system-theme-fg) !important;
    --yt-spec-text-secondary: var(--system-theme-grey2) !important;
    --ytd-searchbox-background: var(--system-theme-bg_dim) !important;
    --ytd-searchbox-legacy-border-color: var(--system-theme-bg0) !important;
    --yt-spec-red-indicator: var(--system-theme-red) !important;
    --yt-spec-static-brand-red: var(--system-theme-red) !important;
    --yt-spec-icon-active-other: var(--system-theme-fg) !important;
    --yt-spec-wordmark-text: var(--system-theme-fg) !important;
    --yt-spec-icon-active-other: var(--system-theme-fg) !important;
    --yt-spec-icon-inactive: var(--system-theme-fg) !important;
    --yt-spec-icon-disabled: var(--system-theme-grey2) !important;
    --yt-spec-brand-icon-inactive: var(--system-theme-fg) !important;
  }

  .yt-spec-button-shape-next--mono.yt-spec-button-shape-next--filled,
  .yt-spec-icon-badge-shape--style-overlay .yt-spec-icon-badge-shape__icon,
  .ytSearchboxComponentSearchButton,
  .ytSearchboxComponentInputBox,
  .ytSearchboxComponentInputBoxDark,
  .ytSearchboxComponentHost,
  .ytSearchboxComponentHostDark, .yt-spec-button-shape-next--overlay.yt-spec-button-shape-next--text,
  .yt-spec-button-shape-next--overlay.yt-spec-button-shape-next--text {
    color: var(--system-theme-fg) !important;
    background-color: var(--system-theme-bg) !important;
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

  .ytChipShapeActive {
    background-color: var(--system-theme-primary) !important;
  }

  /* weird black bands around a "cinimatic video" */
  .ytd-watch-flexy[full-bleed-player] #full-bleed-container.ytd-watch-flexy {
    background-color: var(--system-theme-bg) !important;
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
