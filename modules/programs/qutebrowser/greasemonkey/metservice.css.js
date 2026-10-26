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
    --ms-red-text: color-mix(in srgb, var(--system-theme-red) 15%, var(--system-theme-fg));
    --ms-orange-text: color-mix(in srgb, var(--system-theme-orange) 15%, var(--system-theme-fg));
    --ms-yellow-text: color-mix(in srgb, var(--system-theme-yellow) 15%, var(--system-theme-fg));
    --ms-green-text: color-mix(in srgb, var(--system-theme-green) 15%, var(--system-theme-fg));
    --ms-aqua-text: color-mix(in srgb, var(--system-theme-aqua) 15%, var(--system-theme-fg));
    --ms-blue-text: color-mix(in srgb, var(--system-theme-blue) 15%, var(--system-theme-fg));
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
  .Map,
  .Map-cartography,
  .ImageScrubber-figure,
  #InitialPageLoader,
  .pageLoader-animation {
    background: var(--system-theme-bg_dim) !important;
    color: var(--system-theme-fg) !important;
  }
  html {
    scrollbar-color: var(--system-theme-bg5) var(--system-theme-bg1);
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
  .Nav-scroller {
    scrollbar-width: none !important;
  }
  .Nav-scroller::-webkit-scrollbar {
    display: none !important;
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
  .MobileMenu-list-item.is-active::before,
  .Sidebar-item::before {
    background: var(--system-theme-primary) !important;
  }

  /* The logo sprite paints a fixed near-white fill. Page CSS cannot reach
     a fill inside an external <use> sprite. Move the logo out of its box,
     and show a theme-coloured drop shadow of it at the original place. */
  .Nav-logo .SVG,
  .Footer-section-logo .SVG {
    display: block;
    overflow: hidden;
  }
  .Nav-logo .SVG svg,
  .Footer-section-logo .SVG svg {
    transform: translateX(-1000px);
    filter: drop-shadow(1000px 0 0 var(--system-theme-fg));
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
  .DateRow__header,
  .TileTable-row:nth-child(even) /* no BEM modifier for zebra rows */ {
    background: var(--system-theme-bg2) !important;
    color: var(--system-theme-fg) !important;
  }
  .Modal-Component--overlay {
    background: color-mix(in srgb, var(--system-theme-bg_dim) 80%, transparent) !important;
  }

  /* Accordions (day tiles at narrow widths) and drawers (7-days and other
     routes). The site sets the accordion colours with !important on long
     selectors. :not(#_) adds ID weight, so these rules win. */
  .Accordion-item:not(#_),
  .Accordion-head-wrapper:not(#_),
  .Accordion-head-wrapper:not(#_):hover,
  .Accordion-head-wrapper:not(#_):hover + .Accordion-content-background,
  .Accordion-content-background:not(#_),
  .Accordion-content:not(#_),
  .DrawerMenu-item:not(#_) {
    background: var(--system-theme-bg0) !important;
    color: var(--system-theme-fg) !important;
  }
  .DrawerMenu-item.selected-drawer:not(#_) {
    background: var(--system-theme-bg2) !important;
  }
  .Accordion--expand-icon .SVG:not(#_),
  .DrawerMenu-item .SVG {
    fill: var(--system-theme-fg) !important;
  }

  /* Text that the site greys or navies out */
  .u-textGrey,
  .u-textDefault,
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
  .current-conditions__button,
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

  /* Buttons and sliders. The semicircle slider button rule on the site has
     a type selector and !important, so it needs the doubled class. */
  .Button,
  .Button *,
  .Swiper-button,
  .Swiper-button.Swiper-button--semiCircle,
  .map-controls--btn-group .ZoomControl-btn {
    background: var(--ms-accent-bg) !important;
    color: var(--system-theme-fg) !important;
  }

  /* Map overlays (marker pills, dots, labels, controls) and the icon chips
     in the nearby forecast list. The site sets their colours with inline
     styles. The pill border keeps the site rating colour (good, fair, bad)
     from the inline style. */
  .Pill,
  .pill--point,
  .pill--label-bg,
  .pill--label-padding,
  [data-testid="pw-marker-selector"],
  [data-comp="flex-div-centered"][style*="background-color"],
  .leaflet-bar a,
  .leaflet-control-attribution {
    background: var(--system-theme-bg0) !important;
    color: var(--system-theme-fg) !important;
  }
  .Pill *,
  .pill--label-bg * {
    color: var(--system-theme-fg) !important;
  }

  /* Links */
  .Main a:not(.Button, .Nav-menu-list-item a),
  .Tile-compact-link,
  .u-linkClean,
  .u-textLink,
  .leaflet-control-attribution a {
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

  /* Warnings. Each severity is a solid theme hue, the strongest colour on
     the page. No theme colour reaches 4.5 on every theme hue (for example
     fg on orange in everforest-light is 2.1), so the text is black or white,
     picked from the lightness of the hue: oklch L 0.565 is about relative
     luminance 0.18, where black and white both give 4.5 or more. */
  .is-red {
    --ms-sev: var(--system-theme-red);
  }
  .is-orange {
    --ms-sev: var(--system-theme-orange);
  }
  .is-yellow {
    --ms-sev: var(--system-theme-yellow);
  }
  .is-blue {
    --ms-sev: var(--system-theme-blue);
  }
  .WeatherWarning,
  .SeverityColor,
  .SmallHeaderWarning {
    --ms-on-sev: oklch(from var(--ms-sev) clamp(0, (0.565 - l) * 1000, 1) 0 0);
  }
  .WeatherWarning-div,
  .WeatherWarning-bar {
    background: var(--system-theme-bg2) !important;
    color: var(--system-theme-fg) !important;
  }
  .WeatherWarning:is(.is-red, .is-orange, .is-yellow, .is-blue) .WeatherWarning-div,
  .WeatherWarning:is(.is-red, .is-orange, .is-yellow, .is-blue) .WeatherWarning-bar,
  .SeverityColor:is(.is-red, .is-orange, .is-yellow, .is-blue),
  .SmallHeaderWarning:is(.is-red, .is-orange, .is-yellow, .is-blue) {
    background: var(--ms-sev) !important;
    color: var(--ms-on-sev) !important;
    fill: var(--ms-on-sev) !important;
  }
  .WeatherWarning-bar-warnings,
  .warnings-header-content {
    background: transparent !important;
    border-color: currentColor !important;
  }
  .WeatherWarning-div *,
  .WeatherWarning-bar *,
  .SeverityColor *,
  .SmallHeaderWarning * {
    color: inherit !important;
  }
  .WeatherWarning-body,
  .warning-body-item {
    background: var(--system-theme-bg0) !important;
    color: var(--system-theme-fg) !important;
  }
  .BadgeWithText:is(.is-red, .is-orange, .is-yellow, .is-blue)::before {
    background: var(--ms-sev) !important;
  }
  .Tile-tabs-item:is(.is-red, .is-orange, .is-yellow),
  .Tile-tabs-item:is(.is-red, .is-orange, .is-yellow).is-active {
    box-shadow: inset 0 4px var(--ms-sev) !important;
  }
  .Accordion-head .is-red:not(#_),
  .Accordion-head .is-orange:not(#_),
  .Accordion-head .is-yellow:not(#_),
  .TileList-content.has-warning:is(.is-red, .is-orange, .is-yellow) {
    box-shadow: inset 4px 0 var(--ms-sev) !important;
  }
  .InlineWarning,
  .InlineWarning:hover,
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
  .warning-state-blue {
    color: var(--ms-blue-text) !important;
  }
  .status-medium-good {
    color: var(--ms-aqua-text) !important;
  }

  /* Footer */
  .Footer,
  .Footer-section,
  .Footer-section--global,
  .Footer-promo-container,
  .Footer .Grid-cell {
    background: var(--system-theme-bg_dim) !important;
    color: var(--system-theme-fg) !important;
  }
  .Footer a,
  .Footer-section-links a,
  .Footer-section-legal,
  .Footer-section-social-item {
    color: var(--system-theme-fg) !important;
  }
  .Footer-section-social-item a svg {
    fill: var(--system-theme-fg) !important;
  }
  .Footer-section-social-item a:hover svg {
    fill: var(--ms-accent-text) !important;
  }
`)
