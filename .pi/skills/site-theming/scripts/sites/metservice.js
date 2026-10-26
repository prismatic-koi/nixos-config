// Site config for metservice.com. Read by run-dom-checks.js.
// Run: SITE_CSS=<path to metservice.css.js> node run-dom-checks.js sites/metservice.js
// Routes are the seven routes in the location navigation.
const fs = require('fs');
const path = require('path');

async function expandAll(p) {
  for (let pass = 0; pass < 3; pass++) {
    const n = await p.evaluate(() => {
      let clicked = 0;
      const done = window.__stClicked || (window.__stClicked = new WeakSet());
      const cands = new Set();
      document.querySelectorAll('[data-icon="chevron-down"], [data-icon="chevron-up"], [aria-expanded="false"], .WeatherWarning.is-clickable .WeatherWarning-header, .MoreInformation__header')
        .forEach(i => {
          if (i.closest('.Nav, .Nav--secondary, .SearchBar, .Footer, .Modal-Component, .modal-btn-expander')) return;
          let t = i;
          if (i.matches('[data-icon]')) {
            for (let x = i.parentElement; x && x !== document.body; x = x.parentElement) {
              if (x.matches('button, [role=button], [aria-expanded]') || getComputedStyle(x).cursor === 'pointer') t = x;
              else if (t !== i) break;
            }
            if (t === i) return;
          }
          if (t.closest('a[href]:not([href="#"])')) return;
          cands.add(t);
        });
      // "View More" style text buttons
      document.querySelectorAll('button, [role=button], span, a').forEach(x => {
        if (/^(view|show) more$/i.test(x.textContent.trim()) && !x.closest('a[href]:not([href="#"])')) cands.add(x);
      });
      for (const t of cands) {
        if (done.has(t)) continue; done.add(t);
        // skip ones already open (chevron-up shown)
        if (t.querySelector('[data-icon="chevron-up"]') && !t.querySelector('[data-icon="chevron-down"]')) continue;
        try { t.click(); clicked++; } catch (e) { }
      }
      return clicked;
    });
    await p.waitForTimeout(800);
    if (!n) break;
  }
}


const WARN = fs.readFileSync(path.join(__dirname, 'metservice-warn.js'), 'utf8');

module.exports = {
  name: 'metservice',
  cacheRe: /^https:\/\/(www\.)?metservice\.com\//,
  pages: ['', '7-days', 'extended', 'water-activities', 'nearby-forecasts', 'airborne-allergens', 'past-weather'],
  pageName: (route) => route || '48-hours',
  pageUrl: (route) => 'https://www.metservice.com/towns-cities/regions/wellington/locations/wellington' + (route ? '/' + route : ''),
  readySelector: '.Footer-section--global',
  // In-page check settings. siteColours are "r,g,b" strings of the MetService brand colours.
  checks: {
    siteColours: ['255,255,255', '23,35,68', '29,75,135', '64,123,183', '87,124,231', '117,153,255'],
    themeGradientVars: ['--ms-day', '--ms-night'],
    assetRoots: '.Header, .Nav--primary, .Footer',
    warnRoots: '.SeverityColor, [class*="WeatherWarning-"], .InlineWarning, [class*="warning-state-"]',
    svgWrapper: '.SVG',
  },
  snapshot: {
    navSelector: '.Nav',
    severityClasses: { red: '.is-red', orange: '.is-orange', yellow: '.is-yellow', blue: '.is-blue' },
  },
  // Site colours with chroma 0.08 or more and no status meaning. The theme replaces them with neutral colours.
  exempt: [
    { re: '^(div\\.App-container|section\\.App-primary|div\\.Container|div\\.Container-inner|div\\.Footer-promo-container|section\\.Footer-section\\.Footer-section--promo|div\\.Map)\\b', props: '^(bg|stop)', why: 'page frame: site brand blue, themed as bg_dim' },
    { re: '^section\\.BannerAd\\b', props: '^bg', why: 'empty ad slot: collapsed' },
    { re: '^(nav\\.Nav\\.Nav--secondary|div\\.MobileMenu|ul\\.MobileMenu-list)\\b', props: '^(bg|stop)', why: 'navigation surface: site brand blue, themed as bg1 or the frame colour' },
    { nav: true, re: '^a\\b', props: '^bg', why: 'active top-nav item: site brand blue, the theme marks it with an underline' },
    { re: '^li\\.DrawerMenu-item\\b', props: '^bg', why: 'drawer surface: site brand blue, themed as bg0 or bg2' },
    { re: '^(a\\.Button|button\\.Swiper-button)\\b', props: '^bg', why: 'accent control fill (buttons, slider arrows): no status meaning' },
  ],
  // Open every collapsed control, so that hidden content is checked.
  expand: expandAll,
  // Runs before expand: the collapsed warning bar.
  async before({ p, r }) {
    r.warnCollapsed = await p.evaluate(`(${WARN})()`);
  },
  // Runs after the shared checks. Adds the MetService-only results to r.
  async extras({ p, helper, r, U, pixel }) {
    r.warnExpanded = await p.evaluate(`(${WARN})()`);
    r.nav = await p.evaluate(() => [...document.querySelectorAll('.Nav-scroller, .Nav--secondary .Nav-menu, .Nav--secondary .Nav-menu-list')].map(e => ({ cls: e.className, sbH: e.offsetHeight - e.clientHeight, sbW: e.offsetWidth - e.clientWidth, scrollbarWidth: getComputedStyle(e).scrollbarWidth })));
    r.navScroll = await p.evaluate(() => { const e = document.querySelector('.Nav-scroller'); if (!e) return null; const over = e.scrollWidth > e.clientWidth; e.scrollLeft = 120; const moved = e.scrollLeft; e.scrollLeft = 0; return { overflow: over, scrolledTo: moved }; });
    // Header colour check: distinct pixel colours on a row near the top edge and the bottom edge of each band.
    await p.evaluate(() => scrollTo(0, 0));
    const rects = await p.evaluate(() => { const w = document.documentElement.clientWidth; const n = document.querySelector('.Nav--primary'); const s = document.querySelector('.Nav--secondary-wrapper'); const sr = s && s.getBoundingClientRect(); return { top: n && { x: 0, y: 0, w, h: Math.round(n.getBoundingClientRect().height) }, sec: sr && { x: 0, y: Math.round(sr.top), w, h: Math.round(sr.height) } }; });
    r.header = { top: await pixel.band(p, helper, rects.top), sec: await pixel.band(p, helper, rects.sec) };
    // Ad slots, and text that the unthemed page shows but the themed page hides.
    const textKeys = Object.entries(U.els).filter(([, u]) => u.vis && u.text).map(([k]) => k);
    r.ads = await p.evaluate((textKeys) => {
      const media = (e) => e.querySelectorAll('iframe, img, video').length;
      const ads = [...document.querySelectorAll('.BannerAd, [data-module-name="advert-mod"], .Slot.HasPromoPadding, .Footer-section--promo')].map(e => ({ el: e.tagName.toLowerCase() + '.' + [...e.classList].join('.'), display: getComputedStyle(e).display, text: e.innerText.trim().length, media: media(e) }));
      const byKey = (k) => { let e = document.body; for (const i of k.split('.').slice(1)) e = e && e.children[+i]; return e; };
      const lost = [];
      for (const k of textKeys) { const e = byKey(k); if (e && !e.checkVisibility({ visibilityProperty: true, opacityProperty: true })) lost.push(e.tagName.toLowerCase() + '.' + [...e.classList].slice(0, 3).join('.') + ' ' + e.textContent.trim().slice(0, 40)); }
      return { ads, lost, textChecked: textKeys.length };
    }, textKeys);
  },
  // Pass or fail of the extras, for the summary.
  extraFails(r) {
    const out = [];
    const hdr = Object.values(r.header.top || {});
    if (hdr.some((x) => x.length !== 1)) out.push('header: more than one colour on a row');
    if (r.nav.some((n) => n.sbH > 0 || n.sbW > 0)) out.push('nav scrollbar');
    if (r.ads.lost.length) out.push('hidden text: ' + r.ads.lost.length);
    return out;
  },
};
