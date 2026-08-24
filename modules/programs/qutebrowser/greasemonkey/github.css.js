// ==UserScript==
// @name    Userstyle (github.css)
// @match        *://github.com/*
// ==/UserScript==
GM_addStyle(`
  :root {
    --fontStack-sansSerif: "Noto Sans", sans-serif !important;
    --fontStack-monospace: "JetBrainsMono NF", monospace !important;

    /* bump all the font sizes up, just a little */
    --h00-size-mobile: 2.625rem !important; /* was 2.5rem */
    --h0-size-mobile: 2.125rem !important;  /* was 2rem */
    --h1-size-mobile: 1.75rem !important;   /* was 1.625rem */
    --h2-size-mobile: 1.5rem !important;    /* was 1.375rem */
    --h3-size-mobile: 1.25rem !important;   /* was 1.125rem */
    --h00-size: 3.125rem !important; /* was 3rem */
    --h0-size: 2.625rem !important;  /* was 2.5rem */
    --h1-size: 2.125rem !important;  /* was 2rem */
    --h2-size: 1.625rem !important;  /* was 1.5rem */
    --h3-size: 1.375rem !important;  /* was 1.25rem */
    --h4-size: 1.125rem !important;  /* was 1rem */
    --h5-size: 1rem !important;      /* was 0.875rem */
    --h6-size: 0.875rem !important;  /* was 0.75rem */
    --body-font-size: 1rem !important;      /* was 0.875rem */
    --font-size-small: 0.875rem !important; /* was 0.75rem */

    --borderRadius-full: 5px !important;
    --borderRadius-small: 2px !important;
    --borderRadius-medium: 5px !important;
    --borderRadius-large: 5px !important;
  }

  @media (prefers-color-scheme: dark) {
    [data-color-mode][data-color-mode="auto"][data-dark-theme="dark"], [data-color-mode][data-color-mode="auto"][data-dark-theme="dark"] ::backdrop {
    --bgColor-success-emphasis: var(--system-theme-green) !important;
    --fgColor-success: var(--system-theme-green) !important;
    --fgColor-default: var(--system-theme-fg) !important;
    --fgColor-muted: var(--system-theme-grey1) !important;
    --fgColor-accent: var(--system-theme-primary) !important;
    --fgColor-done: var(--system-theme-purple) !important;
    --fgColor-danger: var(--system-theme-red) !important;
    --fgColor-attention: var(--system-theme-orange) !important;
    --fgColor-onEmphasis: var(--system-theme-bg0) !important;
    --button-primary-fgColor-resting: var(--system-theme-bg0) !important;
    --button-primary-iconColor-rest: var(--system-theme-bg0) !important;
    --button-primary-bgColor-hover: color-mix(in srgb, var(--system-theme-green) 75%, var(--system-theme-bg0) 25%) !important;
    --button-danger-bgColor-hover: color-mix(in srgb, var(--system-theme-red) 75%, var(--system-theme-bg0) 25%) !important;

    --bgColor-default: var(--system-theme-bg0) !important;
    --bgColor-emphasis: var(--system-theme-grey1) !important;
    --bgColor-inset: var(--system-theme-bg_dim) !important;
    --bgColor-muted: var(--system-theme-bg_dim) !important;
    --bgColor-accent-emphasis: var(--system-theme-blue) !important;
    --bgColor-accent-muted: var(--system-theme-bg1) !important;
    --bgColor-danger-emphasis: var(--system-theme-red) !important;
    --bgColor-severe-emphasis: var(--system-theme-orange) !important;
    --bgColor-neutral-emphasis: var(--system-theme-fg) !important;
    --overlay-bgColor: var(--system-theme-bg_dim) !important;
    --control-bgColor-rest: var(--system-theme-bg_dim) !important;

    --borderColor-default: var(--system-theme-bg5) !important;
    --borderColor-success-emphasis: var(--system-theme-green) !important;
    --borderColor-accent-emphasis: var(--system-theme-blue) !important;
    --borderColor-accent-muted: var(--system-theme-blue) !important;
    --borderColor-attention-emphasis: var(--system-theme-orange) !important;
    --borderColor-attention-muted: var(--system-theme-orange) !important;
    --borderColor-danger-emphasis: var(--system-theme-red) !important;
    --borderColor-danger-muted: var(--system-theme-red) !important;
    --bgColor-done-emphasis: var(--system-theme-purple) !important;
    --underlineNav-borderColor-active: var(--system-theme-green) !important;
    --borderColor-translucent: var(--system-theme-bg_dim) !important;

    --button-primary-bgColor-disabled: var(--system-theme-bg_green) !important;
    --button-primary-borderColor-disabled: var(--system-theme-bg1) !important;
    --button-star-iconColor: var(--system-theme-yellow) !important;
    --button-danger-fgColor-rest: var(--system-theme-red) !important;
    --button-primary-bgColor-active: var(--system-theme-green) !important;
    --button-primary-fgColor-rest: var(--system-theme-bg0) !important;

    --tooltip-fgColor: var(--system-theme-fg) !important;
    --tooltip-bgColor: var(--system-theme-bg4) !important;

    --contribution-default-bgColor-4: var(--system-theme-green) !important;
    --contribution-default-bgColor-3: color-mix(in srgb, var(--system-theme-green) 75%, var(--system-theme-bg0) 25%) !important;
    --contribution-default-bgColor-2: color-mix(in srgb, var(--system-theme-green) 50%, var(--system-theme-bg0) 50%) !important;
    --contribution-default-bgColor-1: color-mix(in srgb, var(--system-theme-green) 25%, var(--system-theme-bg0) 75%) !important;
    --contribution-default-bgColor-0: color-mix(in srgb, var(--system-theme-green) 10%, var(--system-theme-bg0) 90%) !important;

    --display-auburn-fgColor: var(--system-theme-red) !important;
    --display-blue-fgColor: var(--system-theme-blue) !important;
    --display-brown-fgColor: var(--system-theme-yellow) !important;
    --display-coral-fgColor: var(--system-theme-red) !important;
    --display-cyan-fgColor: var(--system-theme-aqua) !important;
    --display-gray-fgColor: var(--system-theme-grey2) !important;
    --display-orange-fgColor: var(--system-theme-orange) !important;
    --display-green-fgColor: var(--system-theme-green) !important;
    --display-olive-fgColor: var(--system-theme-green) !important;
    --display-lime-fgColor: var(--system-theme-green) !important;
    --display-pine-fgColor: var(--system-theme-green) !important;
    --display-pink-fgColor: var(--system-theme-purple) !important;
    --display-purple-fgColor: var(--system-theme-purple) !important;
    --display-plum-fgColor: var(--system-theme-purple) !important;
    --display-lemon-fgColor: var(--system-theme-yellow) !important;
    --display-indigo-fgColor: var(--system-theme-blue) !important;

    /* CODE BLOCKS */
    --color-prettylights-syntax-entity: var(--system-theme-orange) !important;
    --color-prettylights-syntax-entity-tag: var(--system-theme-blue) !important;
    --color-prettylights-syntax-string: var(--system-theme-aqua) !important;
    --color-prettylights-syntax-constant: var(--system-theme-purple) !important;
    --color-prettylights-syntax-keyword: var(--system-theme-red) !important;
  }}

  @media (prefers-color-scheme: light) {
    [data-color-mode][data-color-mode="auto"][data-light-theme="light"], [data-color-mode][data-color-mode="auto"][data-light-theme="light"] ::backdrop {
    --bgColor-success-emphasis: var(--system-theme-green) !important;
    --fgColor-success: var(--system-theme-green) !important;
    --fgColor-default: var(--system-theme-fg) !important;
    --fgColor-muted: var(--system-theme-grey1) !important;
    --fgColor-accent: var(--system-theme-primary) !important;
    --fgColor-done: var(--system-theme-purple) !important;
    --fgColor-danger: var(--system-theme-red) !important;
    --fgColor-attention: var(--system-theme-orange) !important;
    --fgColor-onEmphasis: var(--system-theme-bg0) !important;
    --button-primary-fgColor-resting: var(--system-theme-bg0) !important;
    --button-primary-iconColor-rest: var(--system-theme-bg0) !important;
    --button-primary-bgColor-hover: color-mix(in srgb, var(--system-theme-green) 75%, var(--system-theme-bg0) 25%) !important;
    --button-danger-bgColor-hover: color-mix(in srgb, var(--system-theme-red) 75%, var(--system-theme-bg0) 25%) !important;

    --bgColor-default: var(--system-theme-bg0) !important;
    --bgColor-emphasis: var(--system-theme-grey1) !important;
    --bgColor-inset: var(--system-theme-bg_dim) !important;
    --bgColor-muted: var(--system-theme-bg_dim) !important;
    --bgColor-accent-emphasis: var(--system-theme-blue) !important;
    --bgColor-accent-muted: var(--system-theme-bg1) !important;
    --bgColor-danger-emphasis: var(--system-theme-red) !important;
    --bgColor-severe-emphasis: var(--system-theme-orange) !important;
    --bgColor-neutral-emphasis: var(--system-theme-fg) !important;
    --overlay-bgColor: var(--system-theme-bg_dim) !important;
    --control-bgColor-rest: var(--system-theme-bg_dim) !important;

    --borderColor-default: var(--system-theme-bg5) !important;
    --borderColor-success-emphasis: var(--system-theme-green) !important;
    --borderColor-accent-emphasis: var(--system-theme-blue) !important;
    --borderColor-accent-muted: var(--system-theme-blue) !important;
    --borderColor-attention-emphasis: var(--system-theme-orange) !important;
    --borderColor-attention-muted: var(--system-theme-orange) !important;
    --borderColor-danger-emphasis: var(--system-theme-red) !important;
    --borderColor-danger-muted: var(--system-theme-red) !important;
    --bgColor-done-emphasis: var(--system-theme-purple) !important;
    --underlineNav-borderColor-active: var(--system-theme-green) !important;
    --borderColor-translucent: var(--system-theme-bg_dim) !important;

    --button-primary-bgColor-disabled: var(--system-theme-bg_green) !important;
    --button-primary-borderColor-disabled: var(--system-theme-bg1) !important;
    --button-star-iconColor: var(--system-theme-yellow) !important;
    --button-danger-fgColor-rest: var(--system-theme-red) !important;
    --button-primary-bgColor-active: var(--system-theme-green) !important;
    --button-primary-fgColor-rest: var(--system-theme-bg0) !important;

    --tooltip-fgColor: var(--system-theme-fg) !important;
    --tooltip-bgColor: var(--system-theme-bg4) !important;

    --contribution-default-bgColor-4: var(--system-theme-green) !important;
    --contribution-default-bgColor-3: color-mix(in srgb, var(--system-theme-green) 75%, var(--system-theme-bg0) 25%) !important;
    --contribution-default-bgColor-2: color-mix(in srgb, var(--system-theme-green) 50%, var(--system-theme-bg0) 50%) !important;
    --contribution-default-bgColor-1: color-mix(in srgb, var(--system-theme-green) 25%, var(--system-theme-bg0) 75%) !important;
    --contribution-default-bgColor-0: color-mix(in srgb, var(--system-theme-green) 10%, var(--system-theme-bg0) 90%) !important;

    --display-auburn-fgColor: var(--system-theme-red) !important;
    --display-blue-fgColor: var(--system-theme-blue) !important;
    --display-brown-fgColor: var(--system-theme-yellow) !important;
    --display-coral-fgColor: var(--system-theme-red) !important;
    --display-cyan-fgColor: var(--system-theme-aqua) !important;
    --display-gray-fgColor: var(--system-theme-grey2) !important;
    --display-orange-fgColor: var(--system-theme-orange) !important;
    --display-green-fgColor: var(--system-theme-green) !important;
    --display-olive-fgColor: var(--system-theme-green) !important;
    --display-lime-fgColor: var(--system-theme-green) !important;
    --display-pine-fgColor: var(--system-theme-green) !important;
    --display-pink-fgColor: var(--system-theme-purple) !important;
    --display-purple-fgColor: var(--system-theme-purple) !important;
    --display-plum-fgColor: var(--system-theme-purple) !important;
    --display-lemon-fgColor: var(--system-theme-yellow) !important;
    --display-indigo-fgColor: var(--system-theme-blue) !important;

    /* CODE BLOCKS */
    --color-prettylights-syntax-entity: var(--system-theme-orange) !important;
    --color-prettylights-syntax-entity-tag: var(--system-theme-blue) !important;
    --color-prettylights-syntax-string: var(--system-theme-aqua) !important;
    --color-prettylights-syntax-constant: var(--system-theme-purple) !important;
    --color-prettylights-syntax-keyword: var(--system-theme-red) !important;
  }}
`)
