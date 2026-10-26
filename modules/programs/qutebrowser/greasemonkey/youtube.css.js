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

  /* Text colours that keep the hue of a theme colour and reach a 4.5
     contrast ratio. Only the oklch lightness is set (dark when the theme fg
     is light, as in metservice.css.js). The chroma clamp keeps the colour
     inside sRGB. */
  :root {
    --yt-text-l: oklch(from var(--system-theme-fg) calc(0.47 + 0.33 * clamp(0, (l - 0.6) * 1000, 1)) none none);
    --yt-green-text: color-mix(in oklch, var(--yt-text-l), oklch(from var(--system-theme-green) none clamp(0.07, c, 0.1) h));
    --yt-blue-text: color-mix(in oklch, var(--yt-text-l), oklch(from var(--system-theme-blue) none clamp(0.07, c, 0.1) h));
  }

  html, [dark], [light] {
    /* page background */
    --yt-deprecated-general-background-a: var(--system-theme-bg_dim) !important;
    --yt-deprecated-general-background-b: var(--system-theme-bg_dim) !important;
    --yt-deprecated-general-background-c: var(--system-theme-bg_dim) !important;
    /* raised, menu and container surfaces */
    --paper-menu-background-color: var(--system-theme-bg0) !important;
    --paper-listbox-background-color: var(--system-theme-bg0) !important;
    --paper-dialog-background-color: var(--system-theme-bg0) !important;
    --primary-background-color: var(--system-theme-bg0) !important;
    /* text */
    --paper-menu-color: var(--system-theme-fg) !important;
    --paper-menu-disabled-color: var(--system-theme-fg) !important;
    --paper-listbox-color: var(--system-theme-fg) !important;
    --primary-text-color: var(--system-theme-fg) !important;
    --secondary-text-color: var(--system-theme-grey2) !important;
    --disabled-text-color: var(--system-theme-grey2) !important;
    --text-primary-color: var(--system-theme-bg_dim) !important;
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
    --divider-color: var(--system-theme-bg3) !important;
    /* links and call to action */
    --primary-color: var(--system-theme-primary) !important;
    /* status colours */
    --error-color: var(--system-theme-red) !important;
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
  /* body links: the theme blue, set to a lightness with enough contrast */
  .ytAttributedStringLinkCallToActionColor,
  ytd-text-inline-expander a,
  ytd-watch-info-text a {
    color: var(--yt-blue-text) !important;
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
    color: var(--yt-green-text) !important;
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
// YouTube defines each colour token twice: as --yt-sys-color-baseline--<name>
// and as a hashed copy --t<16 hex>. The page rules read the hashed names, and
// the hash changes with YouTube builds. This script does not list the hashed
// names. It reads YouTube's own stylesheets, takes the light and the dark
// value of every hashed name, and finds the baseline name with the same pair
// of values. The hashed name then gets the theme value of that baseline name.
// A hashed name with no baseline match is tried against the inverse pairs
// ("~name": dark value as light, light value as dark). A hashed name whose
// matches map to different theme values is left alone.
const TOKENS = {
  'base-background': 'var(--system-theme-bg_dim)',
  'frosted-glass-desktop': 'var(--system-theme-bg_dim)',
  'frosted-glass-mobile': 'var(--system-theme-bg_dim)',
  'frost-container-medium': 'var(--system-theme-bg_dim)',
  'frost-container-semi-heavy': 'var(--system-theme-bg_dim)',
  'frost-container-heavy': 'var(--system-theme-bg_dim)',
  'themed-overlay-background': 'var(--system-theme-bg_dim)',
  'raised-background': 'var(--system-theme-bg0)',
  'menu-background': 'var(--system-theme-bg0)',
  'surface-container': 'var(--system-theme-bg0)',
  'text-primary': 'var(--system-theme-fg)',
  'inverted-background': 'var(--system-theme-fg)',
  'text-secondary': 'var(--system-theme-grey2)',
  'text-disabled': 'var(--system-theme-grey2)',
  'text-primary-inverse': 'var(--system-theme-bg_dim)',
  'wordmark-text': 'var(--system-theme-fg)',
  'system-bar': 'var(--system-theme-fg)',
  'touch-response': 'var(--system-theme-fg)',
  'mono-filled-hover': 'var(--system-theme-fg)',
  'tonal-background': 'var(--system-theme-bg1)',
  'additive-background': 'var(--system-theme-bg1)',
  'state-mono-standard-hovered': 'var(--system-theme-bg1)',
  'button-chip-background-hover': 'var(--system-theme-bg3)',
  'outline': 'var(--system-theme-bg3)',
  'outline-opaque': 'var(--system-theme-bg3)',
  'suggested-action': 'var(--system-theme-bg_blue)',
  'call-to-action': 'var(--yt-green-text)',
  'call-to-action-inverse': 'var(--yt-green-text)',
  'call-to-action-hover': 'var(--yt-green-text)',
  'static-brand-red': 'var(--system-theme-red)',
  'red-indicator': 'var(--system-theme-red)',
  'error-indicator': 'var(--system-theme-red)',
  'brand-red-contrast': 'var(--system-theme-red)',
  'error-indicator-inverse': 'var(--system-theme-red)',
  'text-error': 'var(--system-theme-red)',
  'text-warning': 'var(--system-theme-yellow)',
  'icon-warning': 'var(--system-theme-yellow)',
  'background-warning': 'var(--system-theme-bg_yellow)',
  'text-complete': 'var(--system-theme-green)',
  'themed-green': 'var(--system-theme-green)',
  'background-complete': 'var(--system-theme-bg_green)',
  '~text-primary': 'var(--system-theme-bg_dim)',
};
const BASE = '--yt-sys-color-baseline--';
const HASHED = /^--t[0-9a-f]{16}$/;
const TOKEN_RULE = /(?:^|[\s,])(?:html|:root|\[light\]|\[dark\])(?![\w-])/;
const base = {};
const hashed = {};
const doneSheets = new WeakSet();
const stats = { sheets: 0, skipped: 0, ms: 0, matched: 0, matches: {}, unmatched: [], ambiguous: [], unmapped: [] };
const tokenStyle = document.createElement('style');
tokenStyle.id = 'yt-theme-tokens';

function readRules(rules) {
  for (const rule of rules) {
    if (rule.cssRules) readRules(rule.cssRules);
    if (!rule.style || !TOKEN_RULE.test(rule.selectorText || '')) continue;
    const sides = [];
    for (const part of rule.selectorText.split(',')) {
      if (!TOKEN_RULE.test(part)) continue;
      sides.push(/\[dark\]/.test(part) ? 'd' : 'l');
    }
    for (const name of rule.style) {
      const isBase = name.startsWith(BASE);
      if (!isBase && !HASHED.test(name)) continue;
      const value = rule.style.getPropertyValue(name).trim().toLowerCase().replace(/\s+/g, '');
      if (!value || value === 'initial' || value.includes('var(')) continue;
      const store = isBase ? base : hashed;
      const key = isBase ? name.slice(BASE.length) : name;
      for (const side of new Set(sides)) (store[key] ||= {})[side] = value;
    }
  }
}

function resolve() {
  const t0 = performance.now();
  const byPair = {};
  const byInverse = {};
  for (const [name, v] of Object.entries(base)) {
    if (!v.l || !v.d) continue;
    (byPair[v.l + '|' + v.d] ||= []).push(name);
    (byInverse[v.d + '|' + v.l] ||= []).push('~' + name);
  }
  const out = [];
  stats.matched = 0;
  stats.matches = {};
  stats.unmatched = [];
  stats.ambiguous = [];
  stats.unmapped = [];
  for (const [name, v] of Object.entries(hashed)) {
    if (!v.l || !v.d) continue;
    const pair = v.l + '|' + v.d;
    let names = byPair[pair] || [];
    if (!names.some((n) => TOKENS[n])) names = names.concat(byInverse[pair] || []);
    if (!names.length) {
      stats.unmatched.push([name, pair]);
      continue;
    }
    const values = new Set(names.filter((n) => TOKENS[n]).map((n) => TOKENS[n]));
    if (values.size === 0) stats.unmapped.push([name, pair, names.join(',')]);
    else if (values.size > 1) stats.ambiguous.push([name, pair, names.join(',')]);
    else {
      out.push(name + ': ' + [...values][0] + ' !important;');
      stats.matched++;
      stats.matches[name] = names.filter((n) => TOKENS[n]);
    }
  }
  for (const name of Object.keys(TOKENS)) {
    if (!name.startsWith('~')) out.push(BASE + name + ': ' + TOKENS[name] + ' !important;');
  }
  tokenStyle.textContent = 'html, [dark], [light] {\n' + out.join('\n') + '\n}';
  if (!tokenStyle.isConnected) document.head.appendChild(tokenStyle);
  stats.ms += performance.now() - t0;
}

let resolvePending = false;
function scheduleResolve() {
  if (resolvePending) return;
  resolvePending = true;
  queueMicrotask(() => {
    resolvePending = false;
    resolve();
  });
}

function readSheet(sheet) {
  if (!sheet || doneSheets.has(sheet)) return;
  doneSheets.add(sheet);
  const t0 = performance.now();
  stats.sheets++;
  try {
    readRules(sheet.cssRules);
  } catch (e) {
    stats.skipped++;
    return;
  }
  stats.ms += performance.now() - t0;
  scheduleResolve();
}

function watchNode(node) {
  if (node.nodeName === 'STYLE') readSheet(node.sheet);
  else if (node.nodeName === 'LINK' && node.rel === 'stylesheet') {
    if (node.sheet) readSheet(node.sheet);
    else node.addEventListener('load', () => readSheet(node.sheet), { once: true });
  }
}

for (const sheet of document.styleSheets) readSheet(sheet);
for (const node of document.head.children) watchNode(node);
new MutationObserver((records) => {
  for (const record of records) for (const node of record.addedNodes) watchNode(node);
}).observe(document.head, { childList: true });
window.__ytThemeStats = stats;

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
