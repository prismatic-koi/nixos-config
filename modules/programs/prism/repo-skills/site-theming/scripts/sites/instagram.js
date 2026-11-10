// Site config for instagram.com. Read by run-dom-checks.js.
// Run: SITE_CSS=<path to instagram.css.js> node run-dom-checks.js sites/instagram.js
// Signed-out pages only: the login page, a public profile, and a public post. The
// profile and the post show a login prompt dialog. Do not use an account.
//
// MODES=light (default) or MODES=dark sets prefers-color-scheme. Instagram follows it
// and sets the class __fb-light-mode or __fb-dark-mode on html. A dark run adds
// "-dark" to the page name of the result files.
//
// Live data: only the static files (scripts, styles, images on static.cdninstagram.com)
// come from the disk cache. A cached page HTML carries old session tokens, and the page
// then shows "Page isn't available". The HTML, the GraphQL data, and the media are live
// in every run, so the profile and post content changes between runs.
const MODE = process.env.MODES || 'light';

// Text that the unthemed page shows and the themed page hides. The key is the text
// as snapshot.js records it (textContent, 40 characters). Live data changes the DOM
// between the two loads, so the match is by text, not by DOM path, and a text counts
// only when the themed page has it in the DOM.
const hiddenTexts = (want) => {
  const shown = new Set(); const present = new Set();
  for (const e of document.body.querySelectorAll('*')) {
    if (/^(SCRIPT|STYLE|NOSCRIPT|TEMPLATE)$/.test(e.tagName) || e.closest('svg')) continue;
    if (![...e.childNodes].some((n) => n.nodeType === 3 && n.textContent.trim())) continue;
    const t = e.textContent.trim().slice(0, 40); present.add(t);
    const r = e.getBoundingClientRect();
    if (r.width > 1 && r.height > 1 && !e.closest('[aria-hidden="true"]') && e.checkVisibility({ visibilityProperty: true, opacityProperty: true })) shown.add(t);
  }
  return want.filter((t) => present.has(t) && !shown.has(t));
};

// Items that a check finds but that the style must not change. Each one moves out of
// the check result into r.exceptions, with the reason. The PR lists the reasons.
//  5b, 5c: user media (photos, videos, profile pictures from the Instagram CDN), and
//          indicators drawn on media (carousel dots). The style does not recolour
//          user content.
//  5c:     on a "-prompt" page, assets outside the dialog. The scrim covers them, and
//          the page without the prompt checks them.
//  6b:     the dialog scrim (a fixed box over the view, while a dialog is open), and
//          divider lines 3 px wide or less.
async function exceptions(p, r) {
  const found = await p.evaluate((cfg) => {
    const path = (e) => { const q = []; for (let i = 0; e && e.nodeType === 1 && i < 4; e = e.parentElement, i++) q.unshift(e.tagName.toLowerCase() + (e.classList.length ? '.' + [...e.classList].slice(0, 3).join('.') : '')); return q.join('>'); };
    // A post video plays from a blob: URL, so every video counts as user media.
    const media = [...document.querySelectorAll('img, video')].filter((m) => m.tagName === 'VIDEO' || (/(cdninstagram\.com|fbcdn\.net)\//.test(m.currentSrc || m.src) && !/static\.cdninstagram\.com/.test(m.currentSrc || m.src))).map((m) => m.getBoundingClientRect()).filter((b) => b.width > 20 && b.height > 20);
    const onMedia = (e) => { const b = e.getBoundingClientRect(); const cx = b.left + b.width / 2, cy = b.top + b.height / 2; return media.some((m) => cx >= m.left && cx <= m.right && cy >= m.top && cy <= m.bottom); };
    const userMedia = (e) => e.tagName === 'IMG' && /(cdninstagram\.com|fbcdn\.net)\//.test(e.currentSrc || e.src) && !/static\.cdninstagram\.com/.test(e.currentSrc || e.src);
    const dialog = document.querySelector('[role=dialog]');
    const prompt = cfg.prompt && !!dialog;
    const out = { b: [], c: {}, scrim: [] };
    // 5b: the elements that the check finds, by the same path form.
    const SITE = new Set(cfg.siteColours);
    for (const e of document.querySelectorAll('*')) {
      const s = getComputedStyle(e);
      const bg = s.backgroundColor.match(/[\d.]+/g);
      if (bg && (bg.length < 4 || +bg[3] > 0) && SITE.has(bg.slice(0, 3).join(',')) && onMedia(e)) out.b.push(path(e));
    }
    // 5c: the runner marks each asset with data-st5c. A re-render can drop the
    // mark, so the reasons are also kept by the path of each img, svg, and sprite.
    const clippedX = (e) => { const b = e.getBoundingClientRect(); for (let x = e.parentElement; x && x !== document.body; x = x.parentElement) { const s = getComputedStyle(x); if (s.overflowX !== 'visible') { const xb = x.getBoundingClientRect(); if (b.right <= xb.left + 1 || b.left >= xb.right - 1) return true; } } return false; };
    const reason = (e) => {
      if (userMedia(e)) return 'user media from the Instagram CDN';
      if (e.closest('button[aria-label="Next"], button[aria-label="Previous"]')) return 'carousel arrow sprite (a dark translucent circle with a white chevron); not painted until the row scrolls, so the screenshot times out';
      if (clippedX(e)) return 'outside a horizontal scroller, so not on screen';
      if (onMedia(e)) return 'icon drawn on user media';
      if (prompt && !e.closest('[role=dialog]')) return 'under the prompt scrim; the page without the prompt checks it';
      return null;
    };
    out.cPath = {};
    out.gone = cfg.errored.filter((p) => ![...document.querySelectorAll('img, svg, div, span, i')].some((e) => path(e) === p));
    for (const e of document.querySelectorAll('img, svg, [data-st5c], button[aria-label="Next"] *, button[aria-label="Previous"] *')) {
      const why = reason(e); if (!why) continue;
      if (e.hasAttribute('data-st5c')) out.c[e.getAttribute('data-st5c')] = why;
      out.cPath[path(e)] = why;
    }
    // 6b: a fixed box over the whole view while a dialog is open.
    if (dialog) for (const e of document.querySelectorAll('*')) {
      const s = getComputedStyle(e); const b = e.getBoundingClientRect();
      if (s.position === 'fixed' && b.width >= innerWidth - 20 && b.height >= innerHeight - 20 && !e.contains(dialog) && !e.querySelector('img, svg, video')) out.scrim.push(path(e));
    }
    return out;
  }, { siteColours: module.exports.checks.siteColours, prompt: /-prompt/.test(r.name), errored: r.c.filter((x) => x.fail && x.error).map((x) => x.el) });
  r.exceptions = [];
  r.b = r.b.filter((x) => { if (x.visible && found.b.includes(x.el)) { r.exceptions.push({ check: '5b', el: x.el, why: 'indicator drawn on user media' }); return false; } return true; });
  // A re-render can drop the data-st5c mark, so user media also matches by its source URL.
  r.c.forEach((item, i) => { if (!found.c[i] && found.cPath[item.el]) found.c[i] = found.cPath[item.el]; });
  r.c.forEach((item, i) => { if (!found.c[i] && item.error && found.gone.includes(item.el)) found.c[i] = 'removed from the page before the screenshot, so not painted (' + item.error.slice(0, 40) + ')'; });
  r.c.forEach((item, i) => { if (!found.c[i] && /^https:\/\/[^/]*(cdninstagram\.com|fbcdn\.net)\//.test(item.icon) && !/static\.cdninstagram\.com/.test(item.icon)) found.c[i] = 'user media from the Instagram CDN'; });
  for (const [i, why] of Object.entries(found.c)) { const item = r.c[+i]; if (item && item.fail) { item.fail = false; item.exception = why; r.exceptions.push({ check: '5c', el: item.el, why }); } }
  r.empty = r.empty.filter((x) => {
    if (found.scrim.includes(x.el)) { r.exceptions.push({ check: '6b', el: x.el, why: 'dialog scrim' }); return false; }
    if (x.w <= 3) { r.exceptions.push({ check: '6b', el: x.el, why: 'divider line, ' + x.w + ' px wide' }); return false; }
    return true;
  });
}

module.exports = {
  name: 'instagram',
  cacheRe: /^https:\/\/static\.cdninstagram\.com\//,
  colorScheme: MODE,
  // A "-prompt" page keeps the login prompt dialog open. The other pages close it,
  // because the open dialog sets aria-hidden on the page content and the checks skip
  // that content. The URL fragment tells expand() which variant to run.
  pages: ['login', 'profile', 'profile-prompt', 'post', 'post-prompt'],
  pageName: (pg) => pg + (MODE === 'light' ? '' : '-' + MODE),
  pageUrl: (pg) => ({
    login: 'https://www.instagram.com/accounts/login/',
    profile: 'https://www.instagram.com/instagram/',
    post: 'https://www.instagram.com/p/DfPQchkG1tg/',
  })[pg.replace(/-prompt$/, '')] + (pg.endsWith('-prompt') ? '#prompt' : ''),
  readySelector: 'footer[role=contentinfo]',
  // Named screenshot regions for screenshot.js. Run it with --expand to close the
  // login prompt on the profile and post pages. The top bar has no stable selector:
  // its bottom edge is 14 px below its "Log In" link. The login page has no top bar.
  regions: {
    header: { clipTo: 'a[href*="/accounts/login"]', pad: 14 },
    dialog: '[role=dialog]',
    footer: 'footer[role=contentinfo]',
    'full-page': 'full',
  },
  checks: {
    // Instagram and Meta base colours (light and dark mode surfaces, primary button
    // blues) as r,g,b. Black is not in the list: Instagram uses it only behind media
    // (the video letterbox), not as a surface.
    siteColours: ['255,255,255', '250,250,250', '248,249,249', '243,245,247', '241,244,247', '240,242,245', '239,239,239', '12,16,20', '37,41,46', '38,38,38', '33,35,40', '43,48,54', '17,17,18', '74,93,249', '65,80,247', '0,149,246', '0,100,224'],
    themeGradientVars: ['--igt-grad-red', '--igt-grad-orange', '--igt-grad-yellow', '--igt-grad-green', '--igt-grad-aqua', '--igt-grad-blue', '--igt-grad-purple'],
    // Every asset on the page. User media and icons on media are exceptions (see above).
    assetRoots: 'body',
    warnRoots: '',
  },
  snapshot: {},
  // Site colours with chroma 0.08 or more and no status meaning. None so far.
  exempt: [],
  // Signed out, the pages have no accordion or drawer. A click on "more" or a
  // post opens the login prompt. Close the prompt, except on a "-prompt" page.
  async expand(p) {
    if (await p.evaluate(() => location.hash === '#prompt')) {
      await p.waitForSelector('[role=dialog]', { timeout: 15000 }).catch(() => { });
      return;
    }
    for (let i = 0; i < 5; i++) {
      const closed = await p.evaluate(() => {
        const d = document.querySelector('[role=dialog]');
        if (!d) return true;
        const x = d.querySelector('[aria-label="Close"]');
        const b = x && (x.closest('[role=button], button') || x.parentElement);
        if (b) b.click();
        return false;
      });
      if (closed) break;
      await p.waitForTimeout(800);
    }
  },
  async extras({ p, helper, r, U, pixel }) {
    const want = [...new Set(Object.values(U.els).filter((u) => u.vis && u.text).map((u) => u.text))];
    r.lost = await p.evaluate(`(${hiddenTexts})(${JSON.stringify(want)})`);
    r.textChecked = want.length;
    // Header check: the top bar of the profile and post pages. It is the outermost
    // box at the top edge, not higher than 120 px, around the "Log In" link.
    await p.evaluate(() => scrollTo(0, 0));
    const rect = await p.evaluate(() => {
      const a = [...document.querySelectorAll('a[href*="/accounts/login"]')].find((x) => x.getBoundingClientRect().top < 80);
      if (!a) return null;
      let bar = null;
      for (let x = a; x && x !== document.body; x = x.parentElement) {
        const b = x.getBoundingClientRect();
        if (b.height > 120) break;
        if (b.top <= 1) bar = x;
      }
      if (!bar) return null;
      return { x: 0, y: 0, w: document.documentElement.clientWidth, h: Math.round(bar.getBoundingClientRect().bottom) };
    });
    r.header = rect ? await pixel.band(p, helper, rect) : null;
    r.modeClass = await p.evaluate(() => (document.documentElement.className.match(/__(fb|ig)-(light|dark)-mode/) || [''])[0]);
    // After many loads, Instagram sends a signed-out profile to the login page (the URL
    // gets "is_from_rle"). Such a run checks the wrong page, so it must fail.
    r.url = p.url();
    r.wantPath = new URL(module.exports.pageUrl(r.name.split('_')[2].replace(/-dark$/, ''))).pathname;
    await exceptions(p, r);
  },
  extraFails(r) {
    const out = [];
    if (r.header && Object.values(r.header).some((x) => x.length !== 1)) out.push('header: more than one colour on a row');
    if (r.lost.length) out.push('hidden text: ' + r.lost.length);
    if (new URL(r.url).pathname !== r.wantPath) out.push('wrong page: ' + r.url);
    if (!r.modeClass.endsWith('-' + MODE + '-mode')) out.push('site mode: html class is "' + r.modeClass + '", not ' + MODE);
    return out;
  },
};
