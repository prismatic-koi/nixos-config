// ==UserScript==
// @name    Userstyle (instagram.css)
// @match        *://*.instagram.com/*
// ==/UserScript==

// Instagram paints with readable custom properties. The --ig-* values are
// bare "r, g, b" lists (the site uses rgb(var(--ig-primary-text))), so a
// var(--system-theme-*) value cannot go into them. This script resolves each
// theme colour to r, g, b at run time and writes the --ig-* values. The other
// readable names (--primary-text, --divider, ...) are full colours.
//
// The html class is __fb-light-mode or __fb-dark-mode (older builds use
// __ig-*). Both modes get the same values, so the page follows the theme in
// both modes. Element classes are hashed and change between releases, so no
// rule here names a class. Hashed selectors that the script needs are read
// from the site stylesheets at run time.
(() => {
  const ROOT = 'html, :root, .__fb-light-mode, .__fb-dark-mode, .__ig-light-mode, .__ig-dark-mode';

  // Resolve a CSS colour expression to [r, g, b, a] (0-255, alpha 0-1).
  const probe = document.createElement('i');
  probe.style.display = 'none';
  const cv = document.createElement('canvas');
  cv.width = cv.height = 1;
  const cx = cv.getContext('2d', { willReadFrequently: true });
  function computed(prop, value) {
    probe.style.setProperty(prop, '');
    probe.style.setProperty(prop, value);
    if (!probe.isConnected) document.documentElement.appendChild(probe);
    return getComputedStyle(probe).getPropertyValue(prop);
  }
  function rgba(expr) {
    cx.clearRect(0, 0, 1, 1);
    cx.fillStyle = 'rgba(0, 0, 0, 0)';
    cx.fillStyle = computed('color', expr);
    cx.fillRect(0, 0, 1, 1);
    const d = cx.getImageData(0, 0, 1, 1).data;
    return [d[0], d[1], d[2], +(d[3] / 255).toFixed(3)];
  }
  const lin = (c) => ((c /= 255) <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
  const lum = (c) => 0.2126 * lin(c[0]) + 0.7152 * lin(c[1]) + 0.0722 * lin(c[2]);
  const ratio = (a, b) => {
    const x = lum(a);
    const y = lum(b);
    return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
  };
  // OKLCH chroma and hue of an [r, g, b] colour.
  function oklch(c) {
    const [r, g, b] = c.slice(0, 3).map(lin);
    const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b);
    const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b);
    const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b);
    const A = 1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s;
    const B = 0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s;
    return { C: Math.hypot(A, B), H: (Math.atan2(B, A) * 180) / Math.PI + 360 };
  }
  // Strings are joined with +, not with template placeholders, so that the
  // file has no dollar-brace sequence that reads as Nix interpolation.
  const v = (n) => 'var(--system-theme-' + n + ')';
  const WHITE = [255, 255, 255, 1];

  // The minimum ratio is 4.6, not 4.5, so that rounding to whole r, g, b
  // values cannot take a colour below 4.5.
  const MIN = 4.6;

  // A colour in a theme hue that reaches MIN against each colour in bgs. It
  // keeps the hue and the chroma (clamped into sRGB) and steps only the oklch
  // lightness away from the backgrounds (design rule 2).
  function hueOn(hue, bgs) {
    const darker = lum(bgs[0]) > 0.18;
    let c = null;
    for (let i = 0; i <= 40; i++) {
      const l = darker ? 0.7 - i * 0.0125 : 0.6 + i * 0.01;
      c = rgba('oklch(from ' + v(hue) + ' ' + l + ' clamp(0.07, c, 0.12) h)');
      if (bgs.every((b) => ratio(c, b) >= MIN)) break;
    }
    return c;
  }
  // Dimmed text: the most grey1 in a mix with fg that still reaches MIN on each
  // colour in bgs. In a palette with a low-contrast fg, this is fg itself.
  function dimText(bgs, most) {
    for (let p = most; p > 0; p -= 5) {
      const c = rgba('color-mix(in oklch, ' + v('grey1') + ' ' + p + '%, ' + v('fg') + ')');
      if (bgs.every((b) => ratio(c, b) >= MIN)) return c;
    }
    return rgba(v('fg'));
  }

  const HUES = ['red', 'orange', 'yellow', 'green', 'aqua', 'blue', 'purple'];
  let hueAngles = {};

  function build() {
    const t = (n) => rgba(v(n));
    const fg = t('fg');
    const bg0 = t('bg0');
    const bg1 = t('bg1');
    const bg2 = t('bg2');
    const bg3 = t('bg3');
    const bg4 = t('bg4');
    const blue = t('blue');
    const red = t('red');
    const green = t('green');
    const purple = t('purple');
    const secText = dimText([bg0, bg1, bg2], 70);
    const terText = dimText([bg0, bg1], 100);
    const link = hueOn('blue', [bg0, bg1, bg2]);
    const redText = hueOn('red', [bg0, bg1]);
    const greenText = hueOn('green', [bg0, bg1]);
    // Instagram draws white text on its blue buttons, and the same white on
    // media. Keep the white, and give the button fill the lightness of the
    // theme blue that carries white text.
    const button = hueOn('blue', [WHITE]);
    const hover = [...fg.slice(0, 3), 0.08];
    const press = [...fg.slice(0, 3), 0.14];
    const blur = [...bg1.slice(0, 3), 0.8];

    // --ig-* names: written as bare "r, g, b" or "r, g, b, a".
    const IG = {
      // surfaces
      'primary-background': bg0,
      'secondary-background': bg1,
      'elevated-background': bg0,
      'secondary-elevated-background': bg1,
      'banner-background': bg0,
      'bubble-background': bg1,
      'dropdown-background': bg1,
      'menu-background-color': bg1,
      'highlight-background': bg2,
      'elevated-highlight-background': bg2,
      'banner-highlight-background': bg2,
      'secondary-button-background': bg2,
      'secondary-button-hover': bg3,
      'tertiary-button-background': bg0,
      'tertiary-button-hover': bg1,
      'tertiary-button-border': bg3,
      'tertiary-button-text': fg,
      'input-disabled-background-color': bg1,
      'incoming-message-bubble': bg1,
      'background-blur': blur,
      'hover-overlay': hover,
      'stroke-prism': bg0,
      // lines
      separator: bg3,
      'elevated-separator': bg3,
      stroke: bg3,
      'focus-stroke': bg4,
      'tablist-border': bg3,
      'text-input-border-prism': bg3,
      'input-border-secondary': bg3,
      'toggle-background-off-prism': bg3,
      'toggle-outline-prism': bg3,
      'toggle-outline-disabled-prism': bg2,
      'stroke-disabled-off-prism': bg2,
      'stroke-disabled-on-prism': bg2,
      // text and icons
      'primary-text': fg,
      'secondary-text': secText,
      'tertiary-text': terText,
      'primary-icon': fg,
      'secondary-icon': secText,
      'tertiary-icon': terText,
      'menu-text-color': fg,
      'tab-underline-active': fg,
      'text-input-border-hover-prism': fg,
      'toggle-background-on-prism': fg,
      'input-border-primary': fg,
      'input-text-and-icon-primary': fg,
      'input-text-and-icon-secondary': secText,
      'secondary-button': fg,
      // links and the primary button: the theme blue
      link,
      'colors-link-text': link,
      'colors-button-borderless-text': link,
      'colors-button-borderless-text--hover': link,
      'colors-button-borderless-text--pressed': link,
      'colors-button-primary-link-text': link,
      'colors-button-primary-link-text--hover': link,
      'colors-button-primary-link-text--pressed': link,
      'primary-button': button,
      'primary-button-hover': button,
      'outgoing-message-bubble': button,
      'colors-button-primary-background': button,
      'colors-button-primary-background--hover': button,
      'colors-button-primary-background--pressed': button,
      'colors-button-primary-text': WHITE,
      'colors-button-secondary-background': bg2,
      'colors-button-secondary-background--hover': bg3,
      'colors-button-secondary-background--pressed': bg3,
      'colors-button-secondary-elevated-background': bg2,
      'colors-button-secondary-text': fg,
      'colors-button-secondary-link-text': fg,
      'colors-button-secondary-link-text--hover': fg,
      'colors-button-secondary-link-text--pressed': fg,
      'colors-button-secondary-text--disabled': secText,
      'colors-button-secondary-link-text--disabled': secText,
      'temporary-highlight': t('bg_blue'),
      'facebook-blue': blue,
      // colours with meaning: kept in their hue family (design rule 1)
      'error-or-destructive': redText,
      badge: red,
      'live-badge': red,
      success: greenText,
      'close-friends-refreshed': green,
      'subscribers-only': purple,
    };
    // Other readable names: full colours. Two Meta design systems use them:
    // the older one (Instagram pages) and a newer one (the login page).
    const FULL = {
      // text and icons
      'primary-text': fg,
      'text-primary': fg,
      'secondary-text': secText,
      'text-secondary': secText,
      'placeholder-text': secText,
      'section-header-text': secText,
      'disabled-text': secText,
      'primary-icon': fg,
      'secondary-icon': secText,
      'placeholder-icon': secText,
      'disabled-icon': secText,
      'list-cell-chevron': secText,
      'text-input-outside-label': fg,
      'text-input-label-focus': secText,
      'input-label-color-inside': secText,
      'inverse-text': bg0,
      accent: fg,
      'toast-text': fg,
      'fb-company-wordmark': fg,
      // surfaces
      'card-background': bg0,
      'card-background-flat': bg0,
      'card-background-filled': bg1,
      'surface-background': bg0,
      'web-wash': bg0,
      wash: bg1,
      'background-primary': bg0,
      'background-emphasized': bg0,
      'background-raised': bg0,
      'background-elevated': bg0,
      'background-secondary': bg1,
      'background-secondary-strong': bg2,
      'background-deemphasized': bg1,
      'popover-background': bg1,
      'popover-card-background': bg0,
      'callout-background': bg1,
      'toast-background': bg1,
      'nav-bar-background': bg0,
      'input-background': bg0,
      'input-background-hover': bg0,
      'input-background-disabled': bg1,
      'comment-background': bg1,
      'secondary-button-background-floating': bg1,
      // a translucent white fill behind the outline buttons
      'secondary-button-background': [...bg0.slice(0, 3), 0],
      'button-filled-secondary-background': bg2,
      'button-filled-secondary-text': fg,
      'button-filled-secondary-icon': fg,
      'hover-overlay': hover,
      'press-overlay': press,
      'non-media-pressed': press,
      // lines
      divider: bg3,
      'media-outer-border': bg3,
      'input-border-color': bg3,
      'input-border-color-hover': fg,
      'border-unfocused': bg3,
      'border-focused': fg,
      'border-primary': fg,
      'border-secondary': bg3,
      'card-border-color': bg3,
      'container-border': bg3,
      'list-border-color': bg3,
      'tile-border-color': bg3,
      'text-input-border-focus': fg,
      'button-secondary-border-color': bg3,
      // links and primary buttons: the theme blue
      'blue-link': link,
      'primary-deemphasized-button-text': link,
      'toast-text-link': link,
      'input-label-color-highlighted': link,
      'secondary-button-text': fg,
      'primary-button-background': button,
      'primary-button-text': WHITE,
      'primary-button-icon': WHITE,
      'button-secondary-emphasized-border-color': link,
      'border-selected': blue,
      'progress-ring-blue-foreground': blue,
      'switch-active': blue,
      'toggle-on-icon': blue,
      'text-badge-info-background': blue,
      'fb-app-wordmark': link,
      // colours with meaning
      negative: redText,
      'event-date': redText,
      'notification-badge': red,
      positive: greenText,
      warning: hueOn('yellow', [bg0, bg1]),
      'status-alert': red,
      'status-positive': green,
    };
    const trip = (c) => (c[3] < 1 ? c.join(', ') : c.slice(0, 3).join(', '));
    const full = (c) => 'rgb(' + c.slice(0, 3).join(' ') + ' / ' + c[3] + ')';
    const lines = [];
    const decl = (n, value) => lines.push(n + ': ' + value + ' !important;');
    for (const [n, c] of Object.entries(IG)) decl('--ig-' + n, trip(c));
    for (const [n, c] of Object.entries(FULL)) decl('--' + n, full(c));
    // Derived colours for the static rules at the end of this file.
    decl('--igt-sec-text', full(secText));
    decl('--igt-on-grad', full(bg0));
    // Gradient stops in each theme hue, with MIN contrast against bg0, so that
    // gradient text (background-clip: text) is readable on the page.
    hueAngles = {};
    for (const h of HUES) {
      decl('--igt-grad-' + h, full(hueOn(h, [bg0])));
      hueAngles[h] = oklch(t(h)).H % 360;
    }
    NAMES = new Set([...Object.keys(IG).map((n) => '--ig-' + n), ...Object.keys(FULL).map((n) => '--' + n)]);
    return lines.join('\n');
  }

  // Map a site gradient with colour stops (the Instagram brand gradient) to
  // the theme hues: each stop goes to the theme hue nearest to it (design
  // rule 1). A gradient with a grey or transparent stop is not changed.
  function themeGradient(value) {
    const resolved = computed('background-image', value);
    const stops = resolved.match(/rgba?\([^)]*\)/g);
    if (!/^linear-gradient\(/.test(resolved) || !stops) return null;
    let out = resolved;
    for (const s of stops) {
      const c = rgba(s);
      const o = oklch(c);
      if (c[3] < 0.5 || o.C < 0.08) return null;
      let best = null;
      let bd = 999;
      for (const [h, a] of Object.entries(hueAngles)) {
        const d = Math.min(Math.abs((o.H % 360) - a), 360 - Math.abs((o.H % 360) - a));
        if (d < bd) {
          bd = d;
          best = h;
        }
      }
      out = out.replace(s, 'var(--igt-grad-' + best + ')');
    }
    return out;
  }

  // Meta theme classes declare the same names on a subtree, for example
  // ".x1h0tdf3.x1h0tdf3, .x1h0tdf3.x1h0tdf3:root". Their names are hashed, so
  // read the selectors at run time from each rule that declares a mapped
  // name, and add them to the selector of the override rule. Rules with the
  // brand gradient get an override rule with the same selector.
  let NAMES = new Set();
  const extra = new Set();
  const gradients = new Map();
  const doneSheets = new WeakSet();
  let tokenStyle = null;
  let body = '';
  let pending = false;
  function readRules(rules) {
    for (const r of rules) {
      if (r.cssRules && !r.selectorText) readRules(r.cssRules);
      if (!r.style || !r.selectorText) continue;
      for (const n of r.style) {
        if (NAMES.has(n)) {
          extra.add(r.selectorText);
          break;
        }
      }
      const bi = r.style.getPropertyValue('background-image');
      if (bi && bi.includes('gradient(') && !gradients.has(r.selectorText)) {
        const g = themeGradient(bi);
        if (g) gradients.set(r.selectorText, g);
      }
    }
  }
  function write() {
    pending = false;
    const sel = [ROOT, ...extra].join(',\n');
    const grads = [...gradients]
      .map(([s, g]) => s + ' {\n  background-image: ' + g + ' !important;\n  color: var(--igt-on-grad) !important;\n}')
      .join('\n');
    tokenStyle.textContent = sel + ' {\n' + body + '\n}\n' + grads;
    probe.remove();
  }
  function readSheet(sheet) {
    if (!sheet || doneSheets.has(sheet) || sheet === tokenStyle.sheet) return;
    doneSheets.add(sheet);
    const before = extra.size + gradients.size;
    try {
      readRules(sheet.cssRules);
    } catch (e) {
      return;
    }
    if (extra.size + gradients.size !== before && !pending) {
      pending = true;
      queueMicrotask(write);
    }
  }
  function watchNode(node) {
    if (node.nodeName === 'STYLE') readSheet(node.sheet);
    else if (node.nodeName === 'LINK' && node.rel === 'stylesheet') {
      if (node.sheet) readSheet(node.sheet);
      node.addEventListener('load', () => readSheet(node.sheet), { once: true });
    }
  }

  // theme.css.js can run after this script. Wait for its variables.
  let tries = 0;
  function apply() {
    if (!getComputedStyle(document.documentElement).getPropertyValue('--system-theme-fg').trim()) {
      if (tries++ < 100) requestAnimationFrame(apply);
      return;
    }
    body = build();
    tokenStyle = document.createElement('style');
    tokenStyle.id = 'instagram-theme-tokens';
    document.documentElement.appendChild(tokenStyle);
    for (const sheet of document.styleSheets) readSheet(sheet);
    write();
    for (const node of document.querySelectorAll('style, link[rel=stylesheet]')) watchNode(node);
    new MutationObserver((records) => {
      for (const r of records) for (const node of r.addedNodes) watchNode(node);
    }).observe(document.documentElement, { childList: true, subtree: true });
  }
  apply();
})();

GM_addStyle(`
  html,
  body {
    background-color: var(--system-theme-bg0) !important;
    color: var(--system-theme-fg) !important;
  }

  /* A disabled button (the login button before input) is drawn at 40%
     opacity, so its text cannot reach 4.5. Show the disabled state with a
     neutral fill at full opacity. The fill and the opacity sit on a child,
     which can carry a Meta theme class, so the rule also sets the child. The
     :not(#igt) term adds id weight, to lift the rule above the theme class
     rules in the token style. */
  [role="button"][aria-disabled="true"]:not(#igt),
  [role="button"][aria-disabled="true"] :not(#igt) {
    opacity: 1 !important;
    --primary-button-background: var(--system-theme-bg2) !important;
    --primary-button-text: var(--igt-sec-text) !important;
    --primary-button-icon: var(--igt-sec-text) !important;
  }

  /* The Meta logo under the login form has fixed fill attributes: blue and
     dark in light mode, light grey in dark mode. Draw it in the text colour
     (design rule 5). */
  svg[aria-label="Meta logo"] path {
    fill: var(--system-theme-fg) !important;
  }
`);
