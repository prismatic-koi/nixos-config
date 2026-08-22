// ==UserScript==
// @name         Userstyle (proton.css)
// @match        *://mail.proton.me/*
// @match        *://calendar.proton.me/*
// @match        *://drive.proton.me/*
// @match        *://account.proton.me/*
// @match        *://pass.proton.me/*
// @match        *://mail.protonmail.com/*
// ==/UserScript==
GM_addStyle(`
  :root, .ui-standard {
    /* Proton emits the identical selector for every polarity, light and
       dark alike — only the values change. One block covers all of them,
       so no prefers-color-scheme media query and no theme-class
       discrimination is needed here. */

    --background-norm: var(--system-theme-bg0) !important;
    --background-weak: var(--system-theme-bg1) !important;
    --background-strong: var(--system-theme-bg2) !important;
    --optional-background-lowered: var(--system-theme-bg_dim) !important;
    --optional-background-elevated: var(--system-theme-bg1) !important;

    --text-norm: var(--system-theme-fg) !important;
    --text-weak: var(--system-theme-grey1) !important;
    --text-hint: var(--system-theme-grey0) !important;
    --text-disabled: var(--system-theme-bg5) !important;
    --text-invert: var(--system-theme-bg0) !important;
    --background-invert: var(--system-theme-fg) !important;

    --border-norm: var(--system-theme-bg4) !important;
    --border-weak: var(--system-theme-bg3) !important;

    --primary: var(--system-theme-primary) !important;
    --interaction-norm: var(--system-theme-primary) !important;
    --interaction-norm-major-1: color-mix(in srgb, var(--system-theme-primary) 85%, var(--system-theme-fg) 15%) !important;
    --interaction-norm-major-2: color-mix(in srgb, var(--system-theme-primary) 70%, var(--system-theme-fg) 30%) !important;
    --interaction-norm-major-3: color-mix(in srgb, var(--system-theme-primary) 55%, var(--system-theme-fg) 45%) !important;
    --interaction-norm-minor-1: color-mix(in srgb, var(--system-theme-primary) 85%, var(--system-theme-bg0) 15%) !important;
    --interaction-norm-minor-2: color-mix(in srgb, var(--system-theme-primary) 70%, var(--system-theme-bg0) 30%) !important;
    --interaction-norm-minor-3: color-mix(in srgb, var(--system-theme-primary) 55%, var(--system-theme-bg0) 45%) !important;
    --interaction-weak: var(--system-theme-bg3) !important;
    --interaction-weak-major-1: color-mix(in srgb, var(--system-theme-bg3) 85%, var(--system-theme-fg) 15%) !important;
    --interaction-weak-major-2: color-mix(in srgb, var(--system-theme-bg3) 70%, var(--system-theme-fg) 30%) !important;
    --interaction-weak-minor-1: color-mix(in srgb, var(--system-theme-bg3) 85%, var(--system-theme-bg0) 15%) !important;
    --interaction-weak-minor-2: color-mix(in srgb, var(--system-theme-bg3) 70%, var(--system-theme-bg0) 30%) !important;

    --primary-contrast: var(--system-theme-bg0) !important;
    --interaction-norm-contrast: var(--system-theme-bg0) !important;
    --interaction-weak-contrast: var(--system-theme-fg) !important;

    --signal-danger: var(--system-theme-red) !important;
    --signal-danger-major-1: color-mix(in srgb, var(--system-theme-red) 85%, var(--system-theme-fg) 15%) !important;
    --signal-danger-minor-1: color-mix(in srgb, var(--system-theme-red) 85%, var(--system-theme-bg0) 15%) !important;
    --signal-danger-contrast: var(--system-theme-bg0) !important;

    --signal-warning: var(--system-theme-orange) !important;
    --signal-warning-major-1: color-mix(in srgb, var(--system-theme-orange) 85%, var(--system-theme-fg) 15%) !important;
    --signal-warning-minor-1: color-mix(in srgb, var(--system-theme-orange) 85%, var(--system-theme-bg0) 15%) !important;
    --signal-warning-contrast: var(--system-theme-bg0) !important;

    --signal-success: var(--system-theme-green) !important;
    --signal-success-major-1: color-mix(in srgb, var(--system-theme-green) 85%, var(--system-theme-fg) 15%) !important;
    --signal-success-minor-1: color-mix(in srgb, var(--system-theme-green) 85%, var(--system-theme-bg0) 15%) !important;
    --signal-success-contrast: var(--system-theme-bg0) !important;

    --signal-info: var(--system-theme-blue) !important;
    --signal-info-major-1: color-mix(in srgb, var(--system-theme-blue) 85%, var(--system-theme-fg) 15%) !important;
    --signal-info-minor-1: color-mix(in srgb, var(--system-theme-blue) 85%, var(--system-theme-bg0) 15%) !important;
    --signal-info-contrast: var(--system-theme-bg0) !important;

    --focus-outline: var(--system-theme-primary) !important;
    --focus-ring: var(--system-theme-primary) !important;

    --optional-link-norm: var(--system-theme-primary) !important;
    --optional-link-hover: color-mix(in srgb, var(--system-theme-primary) 85%, var(--system-theme-fg) 15%) !important;
    --optional-link-active: color-mix(in srgb, var(--system-theme-primary) 70%, var(--system-theme-fg) 30%) !important;

    --optional-scrollbar-thumb-color: var(--system-theme-bg5) !important;

    --optional-shadow-primary-color: var(--system-theme-bg2) !important;
    --optional-mini-calendar-today-color: var(--system-theme-primary) !important;

    --email-item-read-background-color: var(--system-theme-bg0) !important;
    --email-item-read-text-color: var(--system-theme-grey1) !important;
    --email-item-unread-background-color: var(--system-theme-bg1) !important;
    --email-item-unread-text-color: var(--system-theme-fg) !important;
  }

  .ui-prominent {
    --background-norm: var(--system-theme-bg_dim) !important;
  }
`)
