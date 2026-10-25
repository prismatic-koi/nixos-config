// ==UserScript==
// @name    Userstyle (metservice.css)
// @match        *://www.metservice.com/*
// @match        *://metservice.com/*
// ==/UserScript==
GM_addStyle(`
  /* Accent and status text sits on bg0-bg2. Mix each hue toward fg so the
     text keeps its hue and reaches a 4.5 contrast ratio in light and dark. */
  :root {
    --ms-accent-text: color-mix(in srgb, var(--system-theme-primary) 15%, var(--system-theme-fg));
    --ms-accent-bg: color-mix(in srgb, var(--system-theme-primary) 12%, var(--system-theme-bg0));
    --ms-warning-bg: color-mix(in srgb, var(--system-theme-orange) 12%, var(--system-theme-bg0));
    --ms-red-text: color-mix(in srgb, var(--system-theme-red) 15%, var(--system-theme-fg));
    --ms-orange-text: color-mix(in srgb, var(--system-theme-orange) 15%, var(--system-theme-fg));
    --ms-green-text: color-mix(in srgb, var(--system-theme-green) 15%, var(--system-theme-fg));
    --ms-aqua-text: color-mix(in srgb, var(--system-theme-aqua) 15%, var(--system-theme-fg));
  }
  /* Page frame and containers. The site paints these navy, blue or white. */
  html,
  body,
  .App,
  .App-container,
  .App-primary,
  .App-secondary,
  .Main,
  .Container,
  .Container--blue,
  .Container--blue--gradient,
  .Container--secondary,
  .Container-inner,
  .BannerAd,
  .Slot,
  .StickyNav,
  .sticky-headers,
  .sticky-bar-context-handler,
  .sticky-bar-context-primary-section,
  .Map-cartography,
  #InitialPageLoader,
  .pageLoader-animation {
    background: var(--system-theme-bg_dim) !important;
    color: var(--system-theme-fg) !important;
  }
  .Container--blue--gradient::before,
  .Container--blue--gradient::after,
  .Container-inner.SearchContainer::before,
  .Container-inner.SearchContainer::after,
  .Nav--secondary .Container::before,
  .Nav--secondary .Container::after {
    background: none !important;
  }

  /* Navigation */
  .Nav,
  .Nav--primary,
  .Nav--secondary,
  .Nav-scroller,
  .Nav--secondary-wrapper,
  .Nav-menu,
  .Nav-dropdown-target,
  .MobileMenu,
  .MobileMenu-list {
    background: var(--system-theme-bg1) !important;
    color: var(--system-theme-fg) !important;
  }
  .Nav-toggle {
    background: var(--system-theme-bg1) !important;
    color: var(--system-theme-fg) !important;
  }
  /* hamburger bars are <i> elements painted white */
  .Nav-toggle i {
    background: var(--system-theme-fg) !important;
  }
  .Nav a,
  .Nav-menu-list-item a,
  .Nav-dropdown-target-item,
  .Nav-dropdown-trigger,
  .MobileMenu a,
  .MobileMenu-list-item {
    color: var(--system-theme-fg) !important;
  }
  .Nav .Nav-menu-list-item.is-active a {
    background: var(--system-theme-bg2) !important;
    box-shadow: inset 0 -3px 0 var(--system-theme-primary) !important;
    color: var(--system-theme-fg) !important;
  }

  /* Search */
  .SearchBar,
  .SearchBar-input-container,
  .SearchOverlay,
  .SearchBar-favourites-dropdown {
    background: var(--system-theme-bg0) !important;
    color: var(--system-theme-fg) !important;
  }
  .SearchBar input {
    background: transparent !important;
    color: var(--system-theme-fg) !important;
  }
  .SearchBar-actions-find {
    background: transparent !important;
    border-color: var(--system-theme-primary) !important;
    color: var(--system-theme-fg) !important;
  }
  .SearchBar-actions-icon {
    color: var(--system-theme-primary) !important;
  }
  .SearchBar-actions-star {
    color: var(--system-theme-yellow) !important;
  }

  /* Cards, tiles, tables, graphs */
  .Tile,
  .Tile-body,
  .Tile-body-v2,
  .Tile-body-header,
  .Tile-body-content-v2,
  .ImageTile,
  .Grid-cell,
  .Graph,
  .ForecastSlider,
  .ForecastSlider-container,
  .ForecastSlider-wrapper,
  .DateRow,
  .DateRow__body,
  .DailyBreakdown,
  .ExtremesTable,
  .Glossary,
  .Sidebar,
  .Modal-Component--background,
  .Modal-Component--content,
  .Modal-Component--content-graph,
  .MoreInformation,
  .MoreInformation__content,
  .u-mB-md {
    background: var(--system-theme-bg0) !important;
    color: var(--system-theme-fg) !important;
  }
  .DailyBreakdown-period,
  .TileTable-header,
  .TileTable-row:nth-child(even) /* no BEM modifier for zebra rows */ {
    background: var(--system-theme-bg2) !important;
    color: var(--system-theme-fg) !important;
  }
  .Modal-Component--overlay {
    background: color-mix(in srgb, var(--system-theme-bg_dim) 80%, transparent) !important;
  }

  /* Text that the site greys or navies out */
  .u-textGrey,
  .fineprint,
  .Tab-item,
  .TileTable-cell,
  .Tile-body-header__button,
  .MoreInformation__content,
  .MoreInformation__item,
  .ForecastSlider-keyList,
  .ForecastSlider-keyList-item,
  .text-brand-active,
  .warning-state-default,
  .display-active-only,
  .fake-link,
  .DateRow__day,
  h1, h2, h3, h4, p, strong, label, li, td, th {
    color: var(--system-theme-fg) !important;
  }
  .MoreInformation__header,
  .ExtremesTable-diff,
  .Tile-compact-link,
  .IconWithText-icon,
  .Tile-body-header__button--info {
    color: var(--ms-accent-text) !important;
  }
  .u-lg-linebreak-commute {
    background: var(--system-theme-bg3) !important;
  }

  /* Tabs: period tabs (Tile-tabs) and section tabs (Tab) */
  .Tile-tabs,
  .Tile-tabs-item,
  .Tab,
  .Tab-item {
    background: var(--system-theme-bg2) !important;
    color: var(--system-theme-fg) !important;
  }
  .Tile-tabs-item.is-active,
  .Tile-tabs-item[aria-selected="true"],
  .Tab-item.is-active,
  .Tab-item[aria-selected="true"] {
    background: var(--system-theme-bg0) !important;
    box-shadow: inset 0 -2px 0 var(--system-theme-primary) !important;
  }

  /* Buttons and sliders */
  .Button,
  .Button *,
  .Swiper-button {
    background: var(--ms-accent-bg) !important;
    color: var(--system-theme-fg) !important;
  }

  /* Links */
  .Main a:not(.Button, .Nav-menu-list-item a),
  .Tile-compact-link,
  .u-linkClean {
    color: var(--ms-accent-text) !important;
  }

  /* Borders */
  .border,
  .u-bT-1,
  .u-bR-1,
  .u-bB-1,
  .u-bL-1,
  .u-flex,
  .u-pT-0,
  .Tile,
  .Tile-body,
  .SearchBar,
  .MoreInformation__content,
  .TileTable-cell,
  .TileTable-row {
    border-color: var(--system-theme-bg4) !important;
  }

  /* Warnings and alerts: keep the hue family, use the theme hue */
  .WeatherWarning-div,
  .WeatherWarning-header,
  .WeatherWarning-bar-warnings,
  .warnings-header-content {
    background: var(--ms-warning-bg) !important;
    box-shadow: inset 0 -4px 0 var(--system-theme-orange) !important;
    color: var(--system-theme-fg) !important;
  }
  .WeatherWarning-bar-warning-item,
  .WeatherWarning-header *,
  .WeatherWarning-div * {
    color: var(--system-theme-fg) !important;
  }
  .WeatherWarning-body,
  .warning-body-item {
    background: var(--system-theme-bg0) !important;
    color: var(--system-theme-fg) !important;
  }
  .InlineWarning,
  .InlineWarning-leftContainer {
    background: var(--system-theme-bg_yellow) !important;
    color: var(--system-theme-fg) !important;
  }
  .warning-state-red,
  .status-bad {
    color: var(--ms-red-text) !important;
  }
  .warning-state-orange,
  .status-medium {
    color: var(--ms-orange-text) !important;
  }
  .warning-state-green,
  .status-good {
    color: var(--ms-green-text) !important;
  }
  .status-medium-good {
    color: var(--ms-aqua-text) !important;
  }

  /* Footer */
  .Footer,
  .Footer-section,
  .Footer-section--global,
  .Footer-promo-container {
    background: var(--system-theme-bg_dim) !important;
    color: var(--system-theme-fg) !important;
  }
  .Footer a,
  .Footer-section-links a,
  .Footer-section-legal,
  .Footer-section-social-item {
    color: var(--system-theme-fg) !important;
  }
`)
