// ==UserScript==
// @name    Userstyle (metservice.css)
// @match        *://www.metservice.com/*
// @match        *://metservice.com/*
// ==/UserScript==
GM_addStyle(`
  /* Status text (good, fair, bad, links) sits on bg0-bg2. It must keep a
     clear hue and reach a 4.5 contrast ratio. A mix toward fg removes the
     hue, so each colour sets its own oklch lightness and keeps the hue of
     the theme colour. A "none" channel in color-mix() takes the value of
     the other colour: --ms-text-l gives the lightness, the theme hue gives
     chroma and hue. The chroma clamp keeps the colour inside sRGB, so the
     browser does not clip it to another hue.
     Theme mode: fg with oklch L above 0.6 is a dark theme (step 1). */
  :root {
    --ms-accent-text: color-mix(in srgb, var(--system-theme-primary) 15%, var(--system-theme-fg));
    --ms-accent-bg: color-mix(in srgb, var(--system-theme-primary) 12%, var(--system-theme-bg0));
    --ms-text-l: oklch(from var(--system-theme-fg) calc(0.47 + 0.33 * clamp(0, (l - 0.6) * 1000, 1)) none none);
    --ms-red-text: color-mix(in oklch, var(--ms-text-l), oklch(from var(--system-theme-red) none clamp(0.07, c, 0.1) h));
    --ms-orange-text: color-mix(in oklch, var(--ms-text-l), oklch(from var(--system-theme-orange) none clamp(0.07, c, 0.1) h));
    --ms-green-text: color-mix(in oklch, var(--ms-text-l), oklch(from var(--system-theme-green) none clamp(0.07, c, 0.1) h));
    --ms-blue-text: color-mix(in oklch, var(--ms-text-l), oklch(from var(--system-theme-blue) none clamp(0.07, c, 0.1) h));

    /* Day and night stops of the hourly date row, in the theme blue. Day is
       lighter than night in every theme. The date text is dark in light
       themes and light in dark themes, so it reaches 4.5 on both stops. */
    --ms-day: color-mix(in oklch,
      oklch(from var(--system-theme-fg) calc(0.86 - 0.36 * clamp(0, (l - 0.6) * 1000, 1)) none none),
      oklch(from var(--system-theme-blue) none clamp(0.06, c, 0.07) h));
    --ms-night: color-mix(in oklch,
      oklch(from var(--system-theme-fg) calc(0.64 - 0.34 * clamp(0, (l - 0.6) * 1000, 1)) none none),
      oklch(from var(--system-theme-blue) none clamp(0.06, c, 0.1) h));
    --ms-on-daynight: oklch(from var(--system-theme-fg) calc(0.2 + 0.75 * clamp(0, (l - 0.6) * 1000, 1)) c h);
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

  /* Navigation. The top bar is one colour from edge to edge, and the
     location tabs sit on the page frame, as on the site. So the parts
     inside the bars have no background. */
  .Nav--primary,
  .Nav-dropdown-target,
  .MobileMenu,
  .MobileMenu-list {
    background: var(--system-theme-bg1) !important;
    color: var(--system-theme-fg) !important;
  }
  .Nav .Container,
  .Nav-toggle,
  .Nav-menu,
  .Nav-scroller,
  .Nav--secondary,
  .Nav--secondary-wrapper {
    background: transparent !important;
    color: var(--system-theme-fg) !important;
  }
  .Nav-scroller {
    scrollbar-width: none !important;
  }
  .Nav-scroller::-webkit-scrollbar {
    display: none !important;
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
    background: transparent !important;
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
  .Sidebar-wrapper,
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
  /* The script at the end of this file maps the date row gradient to
     --ms-day and --ms-night. */
  .DateRow__day {
    color: var(--ms-on-daynight) !important;
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
  .current-conditions__button,
  h1, h2, h3, h4, p, strong, label, li, td, th {
    color: var(--system-theme-fg) !important;
  }
  .MoreInformation__header,
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
     styles. On nearby forecasts, the pill border and its pointer
     (.pill--point) keep the site colour scale from the inline style. A
     white pointer belongs to a white pill, so it takes the pill colour. */
  .Pill,
  .pill--point[style*="#ffffff" i],
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
  /* Water activity pills show the rating (bad, medium, good) as a solid
     fill in the inline style. Match the site fill and use the theme hue. */
  .Pill[style*="#fa6161" i] {
    --ms-sev: var(--system-theme-red);
  }
  .Pill[style*="#df7b01" i] {
    --ms-sev: var(--system-theme-orange);
  }
  .Pill[style*="#74a30f" i] {
    --ms-sev: var(--system-theme-green);
  }
  .Pill:is([style*="#fa6161" i], [style*="#df7b01" i], [style*="#74a30f" i]) {
    background: var(--ms-sev) !important;
    border-color: var(--ms-sev) !important;
    color: var(--ms-on-sev) !important;
    fill: var(--ms-on-sev) !important;
  }
  .Pill:is([style*="#fa6161" i], [style*="#df7b01" i], [style*="#74a30f" i]) * {
    color: inherit !important;
  }

  /* Links */
  .Main a:not(.Button, .Nav-menu-list-item a),
  .Tile-compact-link,
  .u-linkClean,
  .swiper-wrapper-slide a p,
  .u-textLink,
  .leaflet-control-attribution a,
  .ExtremesTable-diff {
    color: var(--ms-blue-text) !important;
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
  .SmallHeaderWarning,
  .Pill {
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
  .warning-state-blue,
  .status-medium-good {
    color: var(--ms-blue-text) !important;
  }

  /* Ad slots. DNS ad blocking leaves them empty, but the site keeps their
     fixed height. Hide a slot only when it holds no ad frame or image. */
  .BannerAd:not(:has(iframe, img, video)),
  [data-module-name="advert-mod"]:not(:has(iframe, img, video)),
  .Slot.HasPromoPadding:empty,
  .Footer-section--promo:has([data-module-name="advert-mod"]):not(:has(iframe, img, video)) {
    display: none !important;
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

// The site writes the day/night gradient of the hourly date row as an
// inline style on .DateRow__header. The stop positions come from sunrise
// and sunset, so they change with the data. CSS cannot change one colour
// inside an inline gradient, so this script replaces the two site colours
// with --ms-night and --ms-day and keeps the stop positions. The observer
// runs it again when the site writes a new gradient. After the change, the
// style holds no site colour, so a second pass changes nothing.
;(() => {
  const map = [
    [/rgb\(\s*23,\s*35,\s*68\s*\)/g, 'var(--ms-night)'],
    [/rgb\(\s*117,\s*153,\s*255\s*\)/g, 'var(--ms-day)'],
  ]
  const fix = (e) => {
    const old = e.getAttribute('style') || ''
    let s = old
    for (const [re, v] of map) s = s.replace(re, v)
    if (s !== old) e.setAttribute('style', s)
  }
  const scan = (root) => root.querySelectorAll('.DateRow__header[style]').forEach(fix)
  new MutationObserver((records) => {
    for (const r of records) {
      if (r.type === 'attributes') {
        if (r.target.classList.contains('DateRow__header')) fix(r.target)
        continue
      }
      for (const n of r.addedNodes) {
        if (n.nodeType !== 1) continue
        if (n.matches('.DateRow__header')) fix(n)
        scan(n)
      }
    }
  }).observe(document.documentElement, {
    subtree: true,
    childList: true,
    attributes: true,
    attributeFilter: ['style'],
  })
  scan(document)
})()
