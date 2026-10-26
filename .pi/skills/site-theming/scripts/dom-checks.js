// In-page checks, run by run-dom-checks.js: page.evaluate(`(${source})(${JSON.stringify(cfg)})`).
// cfg is the "checks" object of the site config (see sites/). Fields:
//   siteColours        "r,g,b" strings. Site base colours that must not stay (5b).
//   themeGradientVars  CSS variables. A gradient whose stops are all these passes (5b).
//   assetRoots         Selector. Roots that hold white assets, for example the header and footer (5c).
//   warnRoots          Selector. Roots whose icons need 3.0 contrast (5c).
//   svgWrapper         Selector. An inline svg renders inside this wrapper box (5c).
// 5a: text contrast against the effective background. Text on a gradient is measured
//     against every stop. A url() image on the path fails.
// 5b: site colours, or gradients that are not theme gradients, in background-color
//     or background-image, including ::before and ::after.
// 5c: assets in the roots. A pixel scan of a screenshot (done by the runner) finds
//     the dominant colour against the background behind it.
// 6b: a visible element, 40 px high or more, with no visible text or image, and a
//     background that differs from the parent.
(cfg) => {
  const cv = document.createElement('canvas'); cv.width = cv.height = 1;
  const cx = cv.getContext('2d', { willReadFrequently: true });
  const cache = new Map();
  // Parse any CSS colour (rgb, color(srgb), oklab, oklch, color-mix result) via canvas.
  const parse = (s) => {
    if (cache.has(s)) return cache.get(s);
    let out;
    if (!s || s === 'transparent' || s === 'rgba(0, 0, 0, 0)') out = [0, 0, 0, 0];
    else {
      cx.clearRect(0, 0, 1, 1); cx.fillStyle = 'rgba(0,0,0,0)'; cx.fillStyle = s; cx.fillRect(0, 0, 1, 1);
      const d = cx.getImageData(0, 0, 1, 1).data;
      out = [d[0], d[1], d[2], d[3] / 255];
    }
    cache.set(s, out); return out;
  };
  const lin = (c) => { c /= 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; };
  const lum = ([r, g, b]) => 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
  const ratio = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
  const over = (top, bot) => { const a = top[3]; return [0, 1, 2].map(i => top[i] * a + bot[i] * (1 - a)).concat(1); };
  const hex = (c) => '#' + c.slice(0, 3).map(v => Math.round(v).toString(16).padStart(2, '0')).join('');
  const path = (e) => { const p = []; for (let i = 0; e && e.nodeType === 1 && i < 4; e = e.parentElement, i++) p.unshift(e.tagName.toLowerCase() + (e.classList.length ? '.' + [...e.classList].slice(0, 3).join('.') : '')); return p.join('>'); };
  const visible = (e) => {
    const r = e.getBoundingClientRect(); if (r.width <= 1 || r.height <= 1) return false;
    if (e.closest('[aria-hidden="true"]')) return false;
    for (let x = e; x; x = x.parentElement) {
      const s = getComputedStyle(x);
      if (s.display === 'none' || s.visibility === 'hidden' || +s.opacity === 0) return false;
      if (s.clip === 'rect(0px, 0px, 0px, 0px)' || s.clipPath === 'inset(50%)') return false;
      
      if ((s.overflow === 'hidden' || s.overflowY === 'hidden') && x !== e) { const xr = x.getBoundingClientRect(); if (xr.height < 1 || xr.width < 1) return false; if (r.top >= xr.bottom || r.bottom <= xr.top) return false; }
    }
    return true;
  };
  // Effective background: walk ancestors, composite translucent layers, stop at first opaque.
  const gradStops = (bi) => (bi.match(/rgba?\([^)]*\)|color\([^)]*\)|oklch\([^)]*\)|oklab\([^)]*\)/g) || []).map(parse);
  const effBg = (e) => {
    const layers = []; const imgs = []; let stops = null;
    for (let x = e; x; x = x.parentElement) {
      const s = getComputedStyle(x);
      if (s.backgroundImage !== 'none') {
        if (/^linear-gradient\(/.test(s.backgroundImage) && !stops && !layers.length) stops = gradStops(s.backgroundImage);
        else imgs.push(path(x) + ' :: ' + s.backgroundImage.slice(0, 80));
      }
      const c = parse(s.backgroundColor);
      if (c[3] > 0) { layers.push(c); if (c[3] >= 0.999) break; }
    }
    let bg = [255, 255, 255, 1]; // canvas default behind html
    for (let i = layers.length - 1; i >= 0; i--) bg = over(layers[i], bg);
    if (stops) stops = stops.map(st => over(st, bg));
    return { bg, imgs, stops };
  };

  // ---- 5a
  const a = []; const onGradient = [];
  let aCount = 0;
  const seen = new Set();
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  let n;
  while ((n = walker.nextNode())) {
    if (!n.textContent.trim()) continue;
    const e = n.parentElement; if (!e || seen.has(e)) continue; seen.add(e);
    if (/^(SCRIPT|STYLE|NOSCRIPT|TEMPLATE|OPTION)$/.test(e.tagName)) continue;
    if (!visible(e)) continue;
    aCount++;
    const s = getComputedStyle(e);
    let fg = parse(s.color);
    let op = 1; for (let x = e; x; x = x.parentElement) op *= +getComputedStyle(x).opacity;
    const { bg, imgs, stops } = effBg(e);
    const text = n.textContent.trim().slice(0, 40);
    if (stops) {
      const rs = stops.map(st => ratio(over([fg[0], fg[1], fg[2], fg[3] * op], st), st));
      const r = Math.min(...rs);
      const rec = { el: path(e), text, fg: hex(over([fg[0], fg[1], fg[2], fg[3] * op], stops[0])), stops: [...new Set(stops.map(hex))], ratio: +r.toFixed(2), imgs };
      onGradient.push(rec);
      if (r < 4.5 || imgs.length) a.push(rec);
      continue;
    }
    fg = over([fg[0], fg[1], fg[2], fg[3] * op], bg);
    const r = ratio(fg, bg);
    if (r < 4.5 || imgs.length) a.push({ el: path(e), text, fg: hex(fg), bg: hex(bg), ratio: +r.toFixed(2), imgs });
  }

  // ---- 5b
  const SITE = new Set(cfg.siteColours || []);
  const siteRgb = new RegExp('rgb\\((' + [...SITE].map(c => c.split(',').join(', ')).join('|') + ')\\)');
  const b = []; let bCount = 0; const themeGrads = [];
  const probe = (v) => { const d = document.createElement('div'); d.style.backgroundColor = v; document.body.appendChild(d); const c = parse(getComputedStyle(d).backgroundColor); d.remove(); return c; };
  const themeStops = (cfg.themeGradientVars || []).map(v => probe('var(' + v + ')')).filter(c => c[3] > 0);
  const near = (x, y) => Math.max(...[0, 1, 2].map(i => Math.abs(x[i] - y[i]))) <= 1;
  const themeGrad = (bi) => { const st = gradStops(bi); return st.length > 0 && themeStops.length > 0 && st.every(c => themeStops.some(t => near(c, t))); };
  for (const e of document.querySelectorAll('*')) {
    for (const pseudo of [null, '::before', '::after']) {
      const s = getComputedStyle(e, pseudo);
      if (pseudo && (s.content === 'none' || s.content === 'normal')) continue;
      bCount++;
      const c = parse(s.backgroundColor);
      const hit = c[3] > 0 && SITE.has(c.slice(0, 3).join(','));
      const bi = s.backgroundImage;
      let grad = /gradient\(/.test(bi);
      if (grad && themeGrad(bi)) { themeGrads.push({ el: path(e) + (pseudo || ''), stops: [...new Set(gradStops(bi).map(hex))] }); grad = false; }
      const siteInImg = SITE.size > 0 && siteRgb.test(bi);
      if (hit || grad || siteInImg) {
        const vis = visible(e);
        b.push({ el: path(e) + (pseudo || ''), visible: vis, bgColor: hit ? s.backgroundColor : undefined, bgImage: (grad || siteInImg) ? bi.slice(0, 120) : undefined });
      }
    }
  }

  // ---- 5c candidates (pixel analysis is done by the runner from a screenshot)
  const c = [];
  const WARN_ROOTS = cfg.warnRoots || '';
  for (const root of document.querySelectorAll([cfg.assetRoots, WARN_ROOTS].filter(Boolean).join(', ') || 'body')) {
    const warnIcon = !!WARN_ROOTS && root.matches(WARN_ROOTS);
    const items = new Set();
    root.querySelectorAll('img, svg').forEach(x => { if (x.tagName === 'svg' && x.parentElement.closest('svg')) return; items.add(x); });
    root.querySelectorAll('*').forEach(x => { if (/url\(/.test(getComputedStyle(x).backgroundImage)) items.add(x); });
    for (const x of items) {
      if (!visible(x)) continue;
      // An inline <svg> renders inside its .SVG wrapper box; measure that box.
      const box = (x.tagName === 'svg' && cfg.svgWrapper && x.parentElement.matches(cfg.svgWrapper)) ? x.parentElement : x;
      if (box.hasAttribute('data-st5c')) continue;
      const r = box.getBoundingClientRect();
      const { bg } = effBg(box.parentElement || box);
      box.setAttribute('data-st5c', String(c.length));
      c.push({ warn: warnIcon, el: path(x), icon: (x.closest('[data-icon]') || x).getAttribute('data-icon') || x.getAttribute('src') || '', rect: { w: r.width, h: r.height }, bg: hex(bg), bgLum: +lum(bg).toFixed(3) });
    }
  }
  // Recolour probe: any element with a CSS filter, mask or blend mode.
  const filtered = [];
  for (const e of document.querySelectorAll('*')) { const s = getComputedStyle(e); if (s.filter !== 'none' || s.maskImage !== 'none' || s.mixBlendMode !== 'normal') filtered.push(path(e) + ' filter=' + s.filter.slice(0, 60)); }
  // ---- 6b empty boxes
  const empty = [];
  const hasMedia = (e) => [...e.querySelectorAll('img, svg, canvas, iframe, video, picture, object, embed')].some(m => { const r = m.getBoundingClientRect(); return r.width > 4 && r.height > 4; })
    || [e, ...e.querySelectorAll('*')].some(x => /url\(/.test(getComputedStyle(x).backgroundImage));
  const hasText = (e) => { const tw = document.createTreeWalker(e, NodeFilter.SHOW_TEXT); let t; while ((t = tw.nextNode())) { if (t.textContent.trim() && t.parentElement && visible(t.parentElement)) return true; } return false; };
  for (const e of document.body.querySelectorAll('*')) {
    if (e.closest('svg')) continue;
    const r = e.getBoundingClientRect();
    if (r.height < 40 || r.width < 2) continue;
    const s = getComputedStyle(e);
    const own = parse(s.backgroundColor);
    const ownImg = s.backgroundImage !== 'none';
    if (own[3] === 0 && !ownImg) continue;
    const parentBg = effBg(e.parentElement).bg;
    const mine = ownImg ? null : over(own, parentBg);
    if (mine && near(mine, parentBg)) continue;
    if (!visible(e)) continue;
    if (hasText(e) || hasMedia(e)) continue;
    empty.push({ el: path(e), id: e.id || undefined, w: Math.round(r.width), h: Math.round(r.height), y: Math.round(r.top + scrollY), bg: mine ? hex(mine) : s.backgroundImage.slice(0, 60), parentBg: hex(parentBg) });
  }
  return { a, aCount, onGradient, b, bCount, themeGrads, c, filtered, empty };
}

